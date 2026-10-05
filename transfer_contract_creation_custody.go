// Original creation custody has an independently approved namespace birth.
// Portable recovery checks the entire original request/admission closure and
// changes only unsigned physical coordinates, never signed SDK generations.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/urnetwork/connect/protocol"
)

const OriginalContractStoreAttribute = "user.urnetwork.original-contracts.v1"
const OriginalContractStoreSchema = "urnetwork-original-contract-custody-v1"
const OriginalContractStoreLeaseName = ".creation.lock"
const MaximumOriginalContractLeaves = maximumOriginalContractLeaves
const MaximumOriginalContractStoreBytes = maximumOriginalContractStoreBytes

var ErrOriginalContractStoreIdentity = errors.New("original contract physical custody changed")

// SourceGeneration belongs to approved durable custody, not to an SDK manager.
type OriginalContractStoreScope struct {
	DomainHash       [32]byte `json:"domain_hash"`
	ClientId         [16]byte `json:"client_id"`
	PublicKey        [32]byte `json:"public_key"`
	SourceGeneration [16]byte `json:"source_generation"`
}

// The root and lease are explicit original birth; runtime never creates them.
type OriginalContractStoreCheckpoint struct {
	Schema          string                     `json:"schema"`
	Scope           OriginalContractStoreScope `json:"scope"`
	DirectoryDevice uint64                     `json:"directory_device"`
	DirectoryInode  uint64                     `json:"directory_inode"`
	LeaseInode      uint64                     `json:"lease_inode"`
}

// The shared physical record carries no authority to interpret another schema.
type OriginalContractStoreFile = OriginalWorkOutboxFile
type OriginalContractStoreRead = OriginalWorkOutboxRead

// All expected identities are supplied independently of the stored records.
func (self OriginalContractStoreScope) Validate() error {
	if self.DomainHash == ([32]byte{}) || self.ClientId == ([16]byte{}) || self.PublicKey == ([32]byte{}) || self.SourceGeneration == ([16]byte{}) {
		return errors.New("original contract custody approval scope is incomplete")
	}
	return nil
}

// Positive custody loss remains distinct from a failed syscall observation.
func originalContractStoreLoss(message string, cause error) error {
	return errors.Join(ErrOriginalContractStoreIdentity, errors.New(message), cause)
}

// An already retained missing physical name proves loss; other failures do not.
func originalContractStoreObservation(message string, cause error) error {
	if errors.Is(cause, os.ErrNotExist) {
		return originalContractStoreLoss(message, cause)
	}
	return cause
}

// Exact canonical grammar refuses rewritten old or ambiguous birth attributes.
func DecodeOriginalContractStoreCheckpoint(raw []byte, expected OriginalContractStoreScope) (OriginalContractStoreCheckpoint, error) {
	var checkpoint OriginalContractStoreCheckpoint
	if err := expected.Validate(); err != nil {
		return checkpoint, err
	}
	if len(raw) == 0 || len(raw) > 4096 {
		return checkpoint, originalContractStoreLoss("original creation birth exceeds its fixed bound", nil)
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return checkpoint, originalContractStoreLoss("original creation birth cannot decode", err)
	}
	canonical, err := json.Marshal(checkpoint)
	if err != nil || !bytes.Equal(raw, canonical) || checkpoint.Schema != OriginalContractStoreSchema || checkpoint.Scope != expected || checkpoint.DirectoryInode == 0 || checkpoint.LeaseInode == 0 || checkpoint.DirectoryInode == checkpoint.LeaseInode {
		return OriginalContractStoreCheckpoint{}, originalContractStoreLoss("original creation birth or independent scope differs", err)
	}
	return checkpoint, nil
}

// Only the two literal content-addressed original leaf kinds are admitted.
func originalContractStoreName(name string) bool {
	for _, kind := range []string{"request", "admission"} {
		prefix := kind + "-"
		if strings.HasPrefix(name, prefix) && originalWorkOutboxCanonicalName(strings.TrimPrefix(name, prefix)) {
			return true
		}
	}
	return false
}

// A complete physical census cannot hide an unrelated file, hardlink or scope.
func originalContractStoreFiles(files []OriginalContractStoreFile) (map[string]OriginalContractStoreFile, error) {
	if len(files) < 2 || len(files) > maximumOriginalContractLeaves+2 {
		return nil, originalContractStoreLoss("original creation complete census exceeds capacity", nil)
	}
	fileKVs, inodeKVs := map[string]OriginalContractStoreFile{}, map[uint64]bool{}
	var total uint64
	for _, file := range files {
		if _, ok := fileKVs[file.Name]; ok || file.Inode == 0 || inodeKVs[file.Inode] {
			return nil, originalContractStoreLoss("original creation physical members repeat", nil)
		}
		fileKVs[file.Name], inodeKVs[file.Inode] = file, true
		switch file.Name {
		case "":
			if file.Mode != 0700 || file.Bytes != 0 || file.Sha256 != "" {
				return nil, originalContractStoreLoss("original creation root differs", nil)
			}
		case OriginalContractStoreLeaseName:
			if file.Mode != 0600 || file.Bytes != 0 || file.Sha256 != originalWorkOutboxDigest(nil) {
				return nil, originalContractStoreLoss("original creation lease differs", nil)
			}
		default:
			if !originalContractStoreName(file.Name) || file.Mode != 0400 || file.Bytes == 0 || file.Bytes > protocol.MaximumOriginalContractAdmissionBytes || !strings.HasPrefix(file.Sha256, "sha256:") || !originalWorkOutboxCanonicalName(strings.TrimPrefix(file.Sha256, "sha256:")+".json") || file.Bytes > maximumOriginalContractStoreBytes-total {
				return nil, originalContractStoreLoss("original creation leaf is unknown, partial or oversized", nil)
			}
			total += file.Bytes
		}
	}
	root, rootOk := fileKVs[""]
	_, leaseOk := fileKVs[OriginalContractStoreLeaseName]
	if !rootOk || !leaseOk {
		return nil, originalContractStoreLoss("original creation root or lease is absent", nil)
	}
	for _, file := range files {
		if file.Device != root.Device {
			return nil, originalContractStoreLoss("original creation census crosses physical devices", nil)
		}
	}
	return fileKVs, nil
}

// Verify original signatures and response/request closure without choosing scope
// or rewriting any identity from a retained leaf. Unanswered requests may remain.
func VerifyOriginalContractStoreInventory(ctx context.Context, scope OriginalContractStoreScope, files []OriginalContractStoreFile, read OriginalContractStoreRead) error {
	if ctx == nil || read == nil {
		return errors.New("original creation inventory has no bounded reader owner")
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	fileKVs, err := originalContractStoreFiles(files)
	if err != nil {
		return err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.Name == "" || file.Name == OriginalContractStoreLeaseName {
			continue
		}
		raw, err := read(ctx, file.Name)
		if err != nil {
			return err
		}
		if uint64(len(raw)) != file.Bytes || originalWorkOutboxDigest(raw) != file.Sha256 {
			return originalContractStoreLoss("original creation borrowed bytes differ", nil)
		}
		var request protocol.OriginalContractRequest
		kind := "request"
		if strings.HasPrefix(file.Name, "admission-") {
			kind = "admission"
			admission, err := protocol.DecodeOriginalContractAdmission(ctx, raw)
			if err != nil {
				return err
			}
			request, err = protocol.DecodeOriginalContractRequest(ctx, admission.Request)
			if err != nil {
				return err
			}
			name, _ := originalContractLeafName("request", admission.Request)
			original, ok := fileKVs[name]
			if !ok || original.Bytes != uint64(len(admission.Request)) || original.Sha256 != originalWorkOutboxDigest(admission.Request) {
				return originalContractStoreLoss("original admission lost its pre-send request closure", nil)
			}
		} else {
			request, err = protocol.DecodeOriginalContractRequest(ctx, raw)
			if err != nil {
				return err
			}
		}
		name, err := originalContractLeafName(kind, raw)
		if err != nil || name != file.Name || request.DomainHash != scope.DomainHash || request.ClientId != scope.ClientId || request.PublicKey != scope.PublicKey {
			return originalContractStoreLoss("original creation name or independent source scope differs", err)
		}
	}
	return ctx.Err()
}

// Borrow the complete copied originals and return only a new physical anchor.
// Logical namespace birth and all actual SDK generation/signature bytes persist.
func RebindOriginalContractStoreInventory(ctx context.Context, scope OriginalContractStoreScope, originalCheckpoint []byte, originals, targets []OriginalContractStoreFile, read OriginalContractStoreRead) ([]byte, error) {
	checkpoint, err := DecodeOriginalContractStoreCheckpoint(originalCheckpoint, scope)
	if err != nil {
		return nil, err
	}
	originalKVs, err := originalContractStoreFiles(originals)
	if err != nil {
		return nil, err
	}
	targetKVs, err := originalContractStoreFiles(targets)
	if err != nil {
		return nil, err
	}
	if checkpoint.DirectoryDevice != originalKVs[""].Device || checkpoint.DirectoryInode != originalKVs[""].Inode || checkpoint.LeaseInode != originalKVs[OriginalContractStoreLeaseName].Inode || len(originalKVs) != len(targetKVs) {
		return nil, originalContractStoreLoss("original creation restore changed complete physical ancestry", nil)
	}
	for name, original := range originalKVs {
		target, ok := targetKVs[name]
		if !ok || target.Mode != original.Mode || target.Bytes != original.Bytes || target.Sha256 != original.Sha256 {
			return nil, originalContractStoreLoss("original creation restore changed an original logical member", nil)
		}
	}
	if err := VerifyOriginalContractStoreInventory(ctx, scope, targets, read); err != nil {
		return nil, err
	}
	checkpoint.DirectoryDevice, checkpoint.DirectoryInode = targetKVs[""].Device, targetKVs[""].Inode
	checkpoint.LeaseInode = targetKVs[OriginalContractStoreLeaseName].Inode
	return json.Marshal(checkpoint)
}

// Actual startup admits exact prepared physical custody and all signed originals.
// This owner is read-only and is joined before the caller constructs SDK workers.
func ValidateOriginalContractStore(ctx context.Context, directory string, scope OriginalContractStoreScope) (resultErr error) {
	if ctx == nil {
		return errors.New("original creation admission context is absent")
	}
	owner, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	store, err := openOriginalContractStore(owner, directory)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.close()) }()
	if store.checkpoint.Scope != scope {
		return originalContractStoreLoss("original creation prepared scope differs from approval", nil)
	}
	files, err := store.inventory(owner)
	if err != nil {
		return err
	}
	read := func(ctx context.Context, name string) ([]byte, error) { return store.read(ctx, name) }
	if err := VerifyOriginalContractStoreInventory(owner, scope, files, read); err != nil {
		return err
	}
	return store.checkPath(owner)
}
