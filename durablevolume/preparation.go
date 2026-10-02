// Offline preparation is separate from runtime admission. A reviewed plan
// names exact public bytes and physical custody; applying it grants no restart.
package durablevolume

import (
	"context"
	"encoding/json"
	"errors"
	"os"
)

const PreparationRequestSchema = "urnetwork-storage-preparation-request-v1"
const PreparationPlanSchema = "urnetwork-storage-preparation-plan-v1"
const PreparationResultSchema = "urnetwork-storage-preparation-result-v1"
const PreparationFenceSchema = "urnetwork-storage-preparation-fence-v1"
const PreparationAttribute = "user.urnetwork.storage-preparation"

var ErrPreparationUncertain = errors.New("storage preparation requires joined exact-plan readback")

// Instance-local refusal barriers are used only by deterministic package tests.
type preparationHooks struct{ after func(string, string) error }

// Every variable work dimension is independently finite. The initial profile
// also fixes individual custody attributes at 4096 bytes, control records at
// 64 KiB and their complete retained journal at 64 MiB.
type PreparationLimits struct {
	MaxEntries             uint64 `json:"max_entries"`
	MaxBytes               uint64 `json:"max_bytes"`
	MaxDepth               uint64 `json:"max_depth"`
	MaxOwnerAttributes     uint64 `json:"max_owner_attributes"`
	MaxOwnerAttributeBytes uint64 `json:"max_owner_attribute_bytes"`
	MaxPlanBytes           uint64 `json:"max_plan_bytes"`
}

// The command independently selects daemon or owner-local scope. Purpose is
// explicit: this first slice supports fresh only; retained/restore never fall
// back to fresh merely because a target happens to be empty.
type PreparationRequest struct {
	Schema             string             `json:"schema"`
	Purpose            string             `json:"purpose"`
	Scope              string             `json:"scope"`
	MountPath          string             `json:"mount_path"`
	FilesystemUuid     string             `json:"filesystem_uuid"`
	FilesystemType     string             `json:"filesystem_type"`
	MinAvailableBytes  uint64             `json:"min_available_bytes"`
	MinAvailableInodes uint64             `json:"min_available_inodes"`
	RootPath           string             `json:"root_path"`
	MarkerPath         string             `json:"marker_path"`
	LeasePath          string             `json:"lease_path"`
	DeclarationPath    string             `json:"declaration_path"`
	ControlPath        string             `json:"control_path"`
	StagingDirectory   string             `json:"staging_directory"`
	FormerWriterFence  Reference          `json:"former_writer_fence"`
	Limits             PreparationLimits  `json:"limits"`
	Owners             []PreparationOwner `json:"owners"`
}

// Application registries parse Inputs into fixed public schemas. Connect does
// not interpret signing policies or import its application consumers.
type PreparationOwner struct {
	Kind         string          `json:"kind"`
	RelativePath string          `json:"relative_path"`
	Purpose      string          `json:"purpose"`
	Inputs       json.RawMessage `json:"inputs"`
}

// External evidence is an explicit assertion, not proof from a local flock
// that an old or remote writer was stopped. The accepted plan retains its hash.
type PreparationFence struct {
	Schema               string `json:"schema"`
	RootPath             string `json:"root_path"`
	RootInode            uint64 `json:"root_inode"`
	Purpose              string `json:"purpose"`
	FormerWritersStopped bool   `json:"former_writers_stopped"`
	NoPreviousOwnerState bool   `json:"no_previous_owner_state"`
	Evidence             string `json:"evidence"`
}

// Identity is separate from file bytes; byte-identical replacement is not a
// continuation of an acknowledged directory or file generation.
type PreparationIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Mode   uint32 `json:"mode"`
	Uid    uint32 `json:"uid"`
	Gid    uint32 `json:"gid"`
}

// The adapter supplies portable members under its one newly staged namespace.
// The base planner independently reads and binds every source descriptor.
type PreparationFile struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Bytes  uint64 `json:"bytes"`
	Sha256 string `json:"sha256,omitempty"`
}

// Only listed attribute destinations may receive a generated checkpoint. The
// adapter returns exact bytes after target semantic inspection; it cannot
// write them or replace an existing checkpoint through this interface.
type PreparationAttributeSpec struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// Returned checkpoint bytes have one fixed reviewed destination.
type PreparedAttribute struct {
	Spec PreparationAttributeSpec `json:"spec"`
	Raw  []byte                   `json:"raw"`
}

// A fixed adapter builds fresh public bytes in staging and later inspects the
// copied target. It borrows descriptors synchronously, never closes them,
// starts workers, signs, writes the live target or authorizes a restart.
type PreparationAdapter struct {
	Build   func(context.Context, *os.File, string, PreparationOwner) (PreparationOwnerPlan, error)
	Inspect func(context.Context, *os.File, PreparationOwnerPlan) ([]PreparedAttribute, error)
}

// The public semantic census and portable staged files bind one fixed adapter.
type PreparationOwnerPlan struct {
	Owner         PreparationOwner           `json:"owner"`
	StagingName   string                     `json:"staging_name"`
	ExclusiveRoot bool                       `json:"exclusive_root,omitempty"`
	Files         []PreparationFile          `json:"files"`
	Attributes    []PreparationAttributeSpec `json:"attributes"`
	Census        json.RawMessage            `json:"census"`
}

// Every copied file retains both a portable target manifest and its original
// reviewed source inode. Directory modes are recorded explicitly too.
type PreparationSource struct {
	File     PreparationFile     `json:"file"`
	Path     string              `json:"path"`
	Identity PreparationIdentity `json:"identity"`
}

// Plan bytes, including generated public nonces and staged files, are the
// explicit apply input. No timestamp-based approval or implicit latest plan.
type PreparationPlan struct {
	Schema            string                         `json:"schema"`
	Request           Reference                      `json:"request"`
	RequestSha256     string                         `json:"request_sha256"`
	RequestBytes      []byte                         `json:"request_bytes"`
	Root              PreparationIdentity            `json:"root"`
	Directories       map[string]PreparationIdentity `json:"directories"`
	Mount             Mount                          `json:"mount"`
	Filesystem        Filesystem                     `json:"filesystem"`
	Nonce             []byte                         `json:"nonce"`
	Generation        []byte                         `json:"generation"`
	Marker            []byte                         `json:"marker"`
	Lease             []byte                         `json:"lease"`
	Owners            []PreparationOwnerPlan         `json:"owners"`
	Sources           []PreparationSource            `json:"sources"`
	RestartAuthorized bool                           `json:"restart_authorized"`
}

// A completed declaration remains an offline artifact, never restart authority.
type PreparationResult struct {
	Schema            string    `json:"schema"`
	Plan              Reference `json:"plan"`
	Declaration       Reference `json:"declaration"`
	RestartAuthorized bool      `json:"restart_authorized"`
}

// These entrypoints cannot gain owner-local authority by changing request
// contents. Explicit WithHost variants inject facts only for instance tests.
func PlanPreparation(ctx context.Context, request Reference, adapter PreparationAdapter) (PreparationPlan, error) {
	return planPreparation(ctx, request, adapter, defaultHost(), daemonScope)
}

// Injects only kernel facts while retaining daemon parsing and real descriptors.
func PlanPreparationWithHost(ctx context.Context, request Reference, adapter PreparationAdapter, host Host) (PreparationPlan, error) {
	return planPreparation(ctx, request, adapter, host, daemonScope)
}

// Owner-local planning requires its separately selected public entry point.
func PlanOwnerLocalPreparation(ctx context.Context, request Reference, adapter PreparationAdapter) (PreparationPlan, error) {
	return planPreparation(ctx, request, adapter, defaultHost(), ownerLocalScope)
}

// Fact injection cannot authorize daemon policies or change real owner custody.
func PlanOwnerLocalPreparationWithHost(ctx context.Context, request Reference, adapter PreparationAdapter, host Host) (PreparationPlan, error) {
	return planPreparation(ctx, request, adapter, host, ownerLocalScope)
}

// Applies only one accepted daemon plan and closes every owned handle on return.
func ApplyPreparation(ctx context.Context, plan Reference, adapter PreparationAdapter) (PreparationResult, error) {
	return applyPreparation(ctx, plan, adapter, defaultHost(), daemonScope, nil)
}

// Real target publication remains mandatory when facts are supplied by a test.
func ApplyPreparationWithHost(ctx context.Context, plan Reference, adapter PreparationAdapter, host Host) (PreparationResult, error) {
	return applyPreparation(ctx, plan, adapter, host, daemonScope, nil)
}

// Owner-local application never silently reinterprets daemon declarations.
func ApplyOwnerLocalPreparation(ctx context.Context, plan Reference, adapter PreparationAdapter) (PreparationResult, error) {
	return applyPreparation(ctx, plan, adapter, defaultHost(), ownerLocalScope, nil)
}

// The explicit owner-local test seam changes facts, not publication semantics.
func ApplyOwnerLocalPreparationWithHost(ctx context.Context, plan Reference, adapter PreparationAdapter, host Host) (PreparationResult, error) {
	return applyPreparation(ctx, plan, adapter, host, ownerLocalScope, nil)
}
