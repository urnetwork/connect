// Backup inventories describe exact local bytes under an exclusive root lease.
// External legacy-writer fences remain assertions; neither inventory nor restore
// verification grants restart, signed-root rebinding or database restore proof.
package durablevolume

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const InventorySchema = "urnetwork-durable-volume-inventory-v3"
const PhysicalInventorySchema = "urnetwork-durable-volume-physical-inventory-v1"
const MaximumPhysicalInventoryEntries = 32768
const FormerWriterFenceSchema = "urnetwork-durable-volume-former-writer-fence-v1"

// Explicit finite limits bound traversal work, allocation and bytes hashed.
type InventoryLimits struct {
	MaxEntries             uint64 `json:"max_entries"`
	MaxBytes               uint64 `json:"max_bytes"`
	MaxDepth               uint64 `json:"max_depth"`
	MaxOwnerAttributes     uint64 `json:"max_owner_attributes"`
	MaxOwnerAttributeBytes uint64 `json:"max_owner_attribute_bytes"`
}

// The retained external assertion includes legacy writers outside our leases.
// Evidence describes the operator's stop/join fence; this package cannot prove it.
type FormerWriterFence struct {
	Schema               string `json:"schema"`
	RootPath             string `json:"root_path"`
	DeclarationSha256    string `json:"declaration_sha256"`
	LeaseSha256          string `json:"lease_sha256"`
	FormerWritersStopped bool   `json:"former_writers_stopped"`
	Evidence             string `json:"evidence"`
}

// Identity and content are separate; a byte restore can change physical inodes.
type PhysicalRoot struct {
	Device Device `json:"device"`
	Inode  uint64 `json:"inode"`
}

// Names are relative, sorted, bounded and unique. Symlinks and special files
// are refused instead of silently omitted from a plausible complete inventory.
type InventoryEntry struct {
	Physical        *PhysicalRoot        `json:"physical,omitempty"`
	Path            string               `json:"path"`
	Kind            string               `json:"kind"`
	Mode            uint32               `json:"mode"`
	Uid             uint32               `json:"uid"`
	Gid             uint32               `json:"gid"`
	Size            uint64               `json:"size"`
	Sha256          string               `json:"sha256,omitempty"`
	OwnerAttributes []InventoryAttribute `json:"owner_attributes,omitempty"`
}

// Known owner attributes are opaque original bytes. Recording them does not
// validate their signed records, embedded inode bindings or pending commits.
// The selected root's generation is retained separately in RootGeneration.
type InventoryAttribute struct {
	Name   string `json:"name"`
	Value  []byte `json:"value"`
	Sha256 string `json:"sha256"`
}

// This exact manifest is sealed externally by its file hash. Its physical root
// is evidence only; it never replaces historical signed inode/device bindings.
type Inventory struct {
	Schema                   string           `json:"schema"`
	Declaration              Reference        `json:"declaration"`
	MountPath                string           `json:"mount_path"`
	FilesystemUuid           string           `json:"filesystem_uuid"`
	MarkerSha256             string           `json:"marker_sha256"`
	StateRoot                StateRootSpec    `json:"state_root"`
	PhysicalRoot             PhysicalRoot     `json:"physical_root"`
	RootGeneration           string           `json:"root_generation"`
	FormerWriterFence        Reference        `json:"former_writer_fence"`
	Limits                   InventoryLimits  `json:"limits"`
	TotalBytes               uint64           `json:"total_bytes"`
	TotalOwnerAttributes     uint64           `json:"total_owner_attributes"`
	TotalOwnerAttributeBytes uint64           `json:"total_owner_attribute_bytes"`
	Entries                  []InventoryEntry `json:"entries"`
	RestartAuthorized        bool             `json:"restart_authorized"`
}

// A matching local inventory proves no remote database, cross-host or service
// recovery. Existing signed root guards must still admit every resumed owner.
type RestoreVerification struct {
	ExpectedInventory          Reference     `json:"expected_inventory"`
	Observed                   Inventory     `json:"observed"`
	ExactLocalBytesAndMetadata bool          `json:"exact_local_bytes_and_metadata"`
	SamePhysicalRoot           bool          `json:"same_physical_root"`
	SameDeclaration            bool          `json:"same_declaration"`
	SameRootGeneration         bool          `json:"same_root_generation"`
	ExpectedDeclaration        Reference     `json:"expected_declaration"`
	ExpectedStateRoot          StateRootSpec `json:"expected_state_root"`
	ExpectedRootGeneration     string        `json:"expected_root_generation"`
	RestartAuthorized          bool          `json:"restart_authorized"`
}

// The caller cannot accidentally request an unbounded traversal.
func (self InventoryLimits) validate() error {
	return self.validateEntries(10000)
}

// The explicit physical profile counts namespace structure independently of
// owner accounting slots. It includes full native raw custody plus directories.
func (self InventoryLimits) validateEntries(maximumEntries uint64) error {
	if self.MaxEntries == 0 || self.MaxEntries > maximumEntries || self.MaxBytes == 0 || self.MaxBytes > 1024*1024*1024*1024 || self.MaxDepth == 0 || self.MaxDepth > 32 {
		return errors.New("durable inventory requires bounded entries, bytes and depth")
	}
	if self.MaxOwnerAttributes == 0 || self.MaxOwnerAttributes > 10000 || self.MaxOwnerAttributeBytes == 0 || self.MaxOwnerAttributeBytes > 16*1024*1024 {
		return errors.New("durable inventory requires bounded owner attribute count and bytes")
	}
	return nil
}

// Every external report or fence is strict, protected and bound to exact bytes.
func readReference(ctx context.Context, reference Reference, maximum int, target any) error {
	if ctx == nil {
		return errors.New("durable inventory context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !canonical(reference.Path) || !validDigest(reference.Sha256) {
		return errors.New("durable evidence requires an exact path and hash")
	}
	raw, err := readProtectedFileContext(ctx, reference.Path, maximum)
	if err != nil {
		return err
	}
	digest := sha256.New()
	for offset := 0; offset < len(raw); offset += 128 * 1024 {
		if err := ctx.Err(); err != nil {
			return err
		}
		digest.Write(raw[offset:min(offset+128*1024, len(raw))])
	}
	if "sha256:"+hex.EncodeToString(digest.Sum(nil)) != reference.Sha256 {
		return errors.New("durable evidence bytes differ")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(decodeStrict(raw, target), ctx.Err())
}

// The exclusive lease is necessary but does not fence older unguarded writers.
// A separate exact external stop/join assertion is required and retained.
func (self *Owner) Inventory(ctx context.Context, fenceReference Reference, limits InventoryLimits) (Inventory, error) {
	return self.inventoryReport(ctx, fenceReference, limits, InventorySchema)
}

// Explicit physical export retains original leaf generations for separately
// reviewed semantic restore. It grants neither rebinding nor restart authority.
func (self *Owner) InventoryPhysical(ctx context.Context, fenceReference Reference, limits InventoryLimits) (Inventory, error) {
	return self.inventoryReport(ctx, fenceReference, limits, PhysicalInventorySchema)
}

// Both report kinds use the same owned lease, limits and descriptor traversal.
// The selected schema is fixed by the public method, never inferred from data.
func (self *Owner) inventoryReport(ctx context.Context, fenceReference Reference, limits InventoryLimits, schema string) (Inventory, error) {
	if ctx == nil {
		return Inventory{}, errors.New("durable inventory context is required")
	}
	maximumEntries := uint64(10000)
	if schema == PhysicalInventorySchema {
		maximumEntries = MaximumPhysicalInventoryEntries
	}
	if err := errors.Join(ctx.Err(), limits.validateEntries(maximumEntries)); err != nil {
		return Inventory{}, err
	}
	if err := self.borrow(); err != nil {
		return Inventory{}, err
	}
	var result Inventory
	err := func() error {
		if self.access != Snapshot {
			return errors.New("durable inventory requires an exclusive root snapshot lease")
		}
		if beneath(self.rootPath, fenceReference.Path) {
			return errors.New("former-writer fence must stay outside the inventoried root")
		}
		var fence FormerWriterFence
		if err := readReference(ctx, fenceReference, maximumConfigBytes, &fence); err != nil {
			return err
		}
		if fence.Schema != FormerWriterFenceSchema || fence.RootPath != self.rootPath || fence.DeclarationSha256 != self.reference.Sha256 || fence.LeaseSha256 != self.rootSpec.LeaseSha256 || !fence.FormerWritersStopped || len(fence.Evidence) == 0 || len(fence.Evidence) > 4096 {
			return errors.New("former-writer stop fence does not bind this declared root")
		}
		if err := self.check(false); err != nil {
			return err
		}
		result = Inventory{Schema: schema, Declaration: self.reference, MountPath: self.spec.MountPath, FilesystemUuid: self.spec.FilesystemUuid,
			MarkerSha256: self.spec.MarkerSha256, StateRoot: self.rootSpec, FormerWriterFence: fenceReference, Limits: limits}
		if err := self.inventory(ctx, &result); err != nil {
			return err
		}
		return errors.Join(ctx.Err(), self.check(false))
	}()
	if err := self.release(err); err != nil {
		return Inventory{}, err
	}
	return result, nil
}

// Verifies exact local file contents and metadata against a retained manifest.
// A changed physical root is reported, never silently rebound or authorized.
func (self *Owner) VerifyInventory(ctx context.Context, expectedReference, fenceReference Reference, limits InventoryLimits) (RestoreVerification, error) {
	return self.verifyInventory(ctx, expectedReference, fenceReference, limits, false)
}

// Explicitly compares a locally restored root admitted by a separately reviewed
// target declaration. This reports a rebind; it never performs or authorizes one.
func (self *Owner) VerifyReboundInventory(ctx context.Context, expectedReference, fenceReference Reference, limits InventoryLimits) (RestoreVerification, error) {
	return self.verifyInventory(ctx, expectedReference, fenceReference, limits, true)
}

// Ordinary inspection retains exact declaration equality; restore mode is explicit.
func (self *Owner) verifyInventory(ctx context.Context, expectedReference, fenceReference Reference, limits InventoryLimits, allowRebound bool) (RestoreVerification, error) {
	if ctx == nil {
		return RestoreVerification{}, errors.New("durable inventory context is required")
	}
	if err := errors.Join(ctx.Err(), limits.validate()); err != nil {
		return RestoreVerification{}, err
	}
	var expected Inventory
	if err := readReference(ctx, expectedReference, 64*1024*1024, &expected); err != nil {
		return RestoreVerification{}, err
	}
	if expected.Schema != InventorySchema || expected.RestartAuthorized || len(expected.Entries) == 0 || len(expected.Entries) > 10000 {
		return RestoreVerification{}, errors.New("retained inventory scope is invalid")
	}
	if err := expected.Limits.validate(); err != nil {
		return RestoreVerification{}, errors.Join(errors.New("retained inventory limits are invalid"), err)
	}
	if uint64(len(expected.Entries)) > expected.Limits.MaxEntries || expected.TotalBytes > expected.Limits.MaxBytes || expected.TotalOwnerAttributes > expected.Limits.MaxOwnerAttributes || expected.TotalOwnerAttributeBytes > expected.Limits.MaxOwnerAttributeBytes {
		return RestoreVerification{}, errors.New("retained inventory exceeds its declared limits")
	}
	rootGeneration, err := hex.DecodeString(expected.RootGeneration)
	if err != nil || len(rootGeneration) != RootGenerationBytes || expected.StateRoot.RootInode == 0 || expected.StateRoot.RootInode != expected.PhysicalRoot.Inode {
		return RestoreVerification{}, errors.New("retained inventory lacks exact root generation metadata")
	}
	rootDigest := sha256.Sum256(rootGeneration)
	if "sha256:"+hex.EncodeToString(rootDigest[:]) != expected.StateRoot.GenerationSha256 {
		return RestoreVerification{}, errors.New("retained inventory root generation differs from its declaration")
	}
	observed, err := self.Inventory(ctx, fenceReference, limits)
	if err != nil {
		return RestoreVerification{}, err
	}
	if expected.MountPath != observed.MountPath || expected.FilesystemUuid != observed.FilesystemUuid || expected.MarkerSha256 != observed.MarkerSha256 || expected.StateRoot.Path != observed.StateRoot.Path || expected.StateRoot.LeasePath != observed.StateRoot.LeasePath || expected.StateRoot.LeaseSha256 != observed.StateRoot.LeaseSha256 || expected.TotalBytes != observed.TotalBytes || expected.TotalOwnerAttributes != observed.TotalOwnerAttributes || expected.TotalOwnerAttributeBytes != observed.TotalOwnerAttributeBytes {
		return RestoreVerification{}, errors.New("retained inventory declaration or byte count differs")
	}
	if !allowRebound && (expected.Declaration != observed.Declaration || expected.StateRoot != observed.StateRoot || expected.RootGeneration != observed.RootGeneration) {
		return RestoreVerification{}, errors.New("retained inventory requires an explicit reviewed target declaration comparison")
	}
	want, err := json.Marshal(expected.Entries)
	got, gotErr := json.Marshal(observed.Entries)
	if err != nil || gotErr != nil || !bytes.Equal(want, got) {
		return RestoreVerification{}, errors.Join(errors.New("retained inventory file bytes or metadata differ"), err, gotErr)
	}
	return RestoreVerification{ExpectedInventory: expectedReference, Observed: observed, ExactLocalBytesAndMetadata: true, SamePhysicalRoot: expected.PhysicalRoot == observed.PhysicalRoot,
		SameDeclaration: expected.Declaration == observed.Declaration, SameRootGeneration: expected.RootGeneration == observed.RootGeneration,
		ExpectedDeclaration: expected.Declaration, ExpectedStateRoot: expected.StateRoot, ExpectedRootGeneration: expected.RootGeneration}, nil
}
