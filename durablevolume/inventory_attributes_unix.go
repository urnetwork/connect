//go:build linux || darwin

// Custody attributes stay on the same no-follow descriptors as file contents.
// Unknown protocol metadata prevents a misleading complete backup report.
package durablevolume

import (
	"github.com/urnetwork/connect/durablesys"

	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

const ownerAttributeNamespace = "user.urnetwork."
const snapshotAttributePrefix = ownerAttributeNamespace + "snapshot."

// This registry names bounded opaque schemas, without importing owner modules.
// New owners must be reviewed here before an inventory can cover their bytes.
func ownerAttributeLimit(name string) (int, bool) {
	switch name {
	case RootGenerationAttribute:
		return RootGenerationBytes, true
	case ownerAttributeNamespace + "native-journal-custody", ownerAttributeNamespace + "attempt-ledger-custody", PreparationAttribute:
		return 4096, true
	case ownerAttributeNamespace + "validator.requests.v1", ownerAttributeNamespace + "sdk-work.v1":
		return 4096, true
	case ownerAttributeNamespace + "original-contracts.v1", ownerAttributeNamespace + "validator.publications.v1":
		return 4096, true
	}
	if strings.HasPrefix(name, snapshotAttributePrefix) {
		suffix := strings.TrimPrefix(name, snapshotAttributePrefix)
		raw, err := hex.DecodeString(suffix)
		if err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == suffix {
			return 4096, true
		}
	}
	return 0, false
}

// Fixed buffers bound even a filesystem with many unrelated attribute names.
func listInventoryAttributes(file *os.File) ([]string, error) {
	raw := make([]byte, 64*1024)
	count, err := durablesys.ListAttributes(int(file.Fd()), raw)
	runtime.KeepAlive(file)
	if err != nil {
		if errors.Is(err, syscall.ERANGE) {
			return nil, errors.Join(errors.New("inventory attribute-name bound exhausted"), err)
		}
		return nil, inventoryAttributeObservation(err)
	}
	if count < 0 || count > len(raw) {
		return nil, errors.New("inventory attribute-name census exceeded its buffer")
	}
	if count == 0 {
		return nil, nil
	}
	raw = raw[:count]
	if raw[len(raw)-1] != 0 {
		return nil, errors.Join(ErrIdentity, errors.New("inventory attribute-name census is incomplete"))
	}
	names := strings.Split(string(raw[:len(raw)-1]), "\x00")
	sort.Strings(names)
	for index, name := range names {
		if name == "" || index > 0 && names[index-1] == name {
			return nil, errors.Join(ErrIdentity, errors.New("inventory attribute-name census is invalid"))
		}
	}
	return names, nil
}

func readInventoryAttribute(file *os.File, name string, maximum int) ([]byte, error) {
	raw := make([]byte, maximum+1)
	count, err := durablesys.GetAttribute(int(file.Fd()), name, raw)
	runtime.KeepAlive(file)
	if err != nil {
		if errors.Is(err, syscall.ERANGE) {
			return nil, errors.Join(errors.New("inventory owner attribute exceeds its reviewed byte bound"), err)
		}
		if errors.Is(err, durablesys.ErrNoAttribute) {
			return nil, errors.Join(ErrIdentity, errors.New("inventory attribute disappeared after enumeration"), err)
		}
		return nil, inventoryAttributeObservation(err)
	}
	if count < 0 || count > maximum {
		return nil, errors.New("inventory owner attribute exceeds its reviewed byte bound")
	}
	return raw[:count], nil
}

// Observation failure refuses this report without poisoning unchanged custody.
func inventoryAttributeObservation(err error) error {
	if err == nil {
		return nil
	}
	if durablesys.AttributeUnsupported(err) {
		return errors.Join(ErrUnsupported, unavailableObservation("inventory attributes are unsupported", err))
	}
	return unavailableObservation("inventory attributes could not be observed", err)
}

func (self *Owner) inventoryAttributeAdmission(ctx context.Context, stage string, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	if self.observeFile != nil {
		err = self.observeFile(stage, file, file.Name())
	}
	return errors.Join(ctx.Err(), inventoryAttributeObservation(err))
}

// The selected root's nonce already has a separate field and external binding.
// Descendant nonces, if present, are retained as opaque owner metadata as well.
func (self *Owner) inventoryAttributes(ctx context.Context, file *os.File, relative string, result *Inventory) ([]InventoryAttribute, error) {
	if err := self.inventoryAttributeAdmission(ctx, "inventory-attributes-list", file); err != nil {
		return nil, err
	}
	names, err := listInventoryAttributes(file)
	if err != nil {
		return nil, err
	}
	var attributes []InventoryAttribute
	rootGenerationSeen := false
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(name, ownerAttributeNamespace) {
			continue
		}
		maximum, known := ownerAttributeLimit(name)
		if !known {
			return nil, fmt.Errorf("inventory cannot cover unknown owner attribute %q", name)
		}
		isRootGeneration := relative == "" && name == RootGenerationAttribute
		if !isRootGeneration && result.TotalOwnerAttributes >= result.Limits.MaxOwnerAttributes {
			return nil, errors.New("inventory owner attribute count bound exhausted")
		}
		if err := self.inventoryAttributeAdmission(ctx, "inventory-attribute-read", file); err != nil {
			return nil, err
		}
		raw, err := readInventoryAttribute(file, name, maximum)
		if err != nil {
			return nil, err
		}
		if isRootGeneration {
			generation, err := hex.DecodeString(result.RootGeneration)
			if err != nil || !bytes.Equal(generation, raw) {
				return nil, errors.Join(ErrIdentity, errors.New("inventory root nonce changed during attribute traversal"), err)
			}
			rootGenerationSeen = true
			continue
		}
		if uint64(len(raw)) > result.Limits.MaxOwnerAttributeBytes-result.TotalOwnerAttributeBytes {
			return nil, errors.New("inventory owner attribute byte bound exhausted")
		}
		digest := sha256.Sum256(raw)
		attributes = append(attributes, InventoryAttribute{Name: name, Value: raw, Sha256: "sha256:" + hex.EncodeToString(digest[:])})
		result.TotalOwnerAttributes++
		result.TotalOwnerAttributeBytes += uint64(len(raw))
	}
	if relative == "" && !rootGenerationSeen {
		return nil, errors.Join(ErrIdentity, errors.New("inventory root nonce disappeared from attribute census"))
	}
	return attributes, ctx.Err()
}
