//go:build linux

// Physical source reports preserve original inode-bearing owner censuses.
// Strict loading authenticates the reviewed bytes; it does not grant a writer.
package durablevolume

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
)

// Retained semantic adapters may consume only an exact physical source report.
// Ordinary v3 inventory lacks leaf generations and cannot fill them by guess.
func LoadPhysicalInventory(ctx context.Context, reference Reference) (Inventory, error) {
	var report Inventory
	if err := readReference(ctx, reference, 64*1024*1024, &report); err != nil {
		return Inventory{}, err
	}
	if err := validatePhysicalInventory(ctx, report); err != nil {
		return Inventory{}, err
	}
	return report, nil
}

// Every dimension, original generation and opaque custody value is checked
// before an adapter receives any report as exact retained source authority.
func validatePhysicalInventory(ctx context.Context, report Inventory) error {
	if ctx == nil {
		return errors.New("physical inventory requires cancellation context")
	}
	if err := errors.Join(ctx.Err(), report.Limits.validateEntries(MaximumPhysicalInventoryEntries)); err != nil {
		return err
	}
	if report.Schema != PhysicalInventorySchema || report.RestartAuthorized || len(report.Entries) == 0 || uint64(len(report.Entries)) > report.Limits.MaxEntries || report.StateRoot.RootInode == 0 || report.StateRoot.RootInode != report.PhysicalRoot.Inode || !canonical(report.StateRoot.Path) || !canonical(report.Declaration.Path) || !validDigest(report.Declaration.Sha256) || !validDigest(report.StateRoot.GenerationSha256) {
		return errors.New("physical inventory lacks exact bounded source authority")
	}
	nonce, err := hex.DecodeString(report.RootGeneration)
	if err != nil || len(nonce) != RootGenerationBytes || hex.EncodeToString(nonce) != report.RootGeneration || preparationDigest(nonce) != report.StateRoot.GenerationSha256 {
		return errors.New("physical inventory original root generation differs")
	}
	var totalBytes, totalAttributes, totalAttributeBytes uint64
	paths := map[string]string{}
	inodes := map[PhysicalRoot]bool{}
	for i, entry := range report.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Physical == nil || entry.Physical.Inode == 0 || entry.Physical.Device != report.PhysicalRoot.Device || inodes[*entry.Physical] || i > 0 && report.Entries[i-1].Path >= entry.Path {
			return errors.New("physical inventory has absent, aliased or unordered original member generations")
		}
		inodes[*entry.Physical] = true
		if i == 0 {
			if entry.Path != "" || entry.Kind != "directory" || *entry.Physical != report.PhysicalRoot {
				return errors.New("physical inventory root entry differs")
			}
		} else {
			if !preparationRelative(entry.Path, report.Limits.MaxDepth, false) || paths[filepath.Dir(entry.Path)] != "directory" && filepath.Dir(entry.Path) != "." {
				return errors.New("physical inventory member lacks its original parent")
			}
		}
		paths[entry.Path] = entry.Kind
		switch entry.Kind {
		case "directory":
			if entry.Mode != 0700 || entry.Size != 0 || entry.Sha256 != "" {
				return errors.New("physical directory metadata is invalid")
			}
		case "file":
			if entry.Mode != 0600 && entry.Mode != 0400 || !validDigest(entry.Sha256) || entry.Size > report.Limits.MaxBytes-totalBytes {
				return errors.New("physical file metadata is invalid or unbounded")
			}
			totalBytes += entry.Size
		default:
			return errors.New("physical inventory contains unsupported member kind")
		}
		prior := ""
		for _, attribute := range entry.OwnerAttributes {
			if err := ctx.Err(); err != nil {
				return err
			}
			limit, known := ownerAttributeLimit(attribute.Name)
			if !known || attribute.Name == RootGenerationAttribute && entry.Path == "" || !strings.HasPrefix(attribute.Name, ownerAttributeNamespace) || attribute.Name <= prior || len(attribute.Value) > limit || preparationDigest(attribute.Value) != attribute.Sha256 || totalAttributes >= report.Limits.MaxOwnerAttributes || uint64(len(attribute.Value)) > report.Limits.MaxOwnerAttributeBytes-totalAttributeBytes {
				return errors.New("physical inventory owner attribute census is invalid or unbounded")
			}
			prior = attribute.Name
			totalAttributes++
			totalAttributeBytes += uint64(len(attribute.Value))
		}
	}
	if totalBytes != report.TotalBytes || totalAttributes != report.TotalOwnerAttributes || totalAttributeBytes != report.TotalOwnerAttributeBytes {
		return errors.New("physical inventory retained totals differ")
	}
	return ctx.Err()
}
