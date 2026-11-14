// A reviewed cohort binds all root plans into one resumable offline operation.
// Each root keeps its original journal; no partial result grants restart.
package durablevolume

import "context"

const PreparationCohortSchema = "urnetwork-storage-preparation-cohort-v1"
const PreparationCohortResultSchema = "urnetwork-storage-preparation-cohort-result-v1"

// These aggregate limits are independent of each member's unchanged limits.
// The maximum profile admits 256 roots but never promises every count/byte
// maximum fits simultaneously. Approval covers all dimensions before effects.
type PreparationCohortLimits struct {
	MaxRoots               uint64 `json:"max_roots"`
	MaxPlanBytes           uint64 `json:"max_plan_bytes"`
	MaxControlBytes        uint64 `json:"max_control_bytes"`
	MaxEntries             uint64 `json:"max_entries"`
	MaxBytes               uint64 `json:"max_bytes"`
	MaxOwnerAttributes     uint64 `json:"max_owner_attributes"`
	MaxOwnerAttributeBytes uint64 `json:"max_owner_attribute_bytes"`
}

// Plans are sorted by original logical root, not filesystem or completion
// order. The exact cohort digest is retained in every new root reservation.
type PreparationCohort struct {
	Schema              string                  `json:"schema"`
	Scope               string                  `json:"scope"`
	RetainedDeclaration *Reference              `json:"retained_declaration,omitempty"`
	Limits              PreparationCohortLimits `json:"limits"`
	Plans               []Reference             `json:"plans"`
}

// Complete confirms every retained journal. Only a complete cohort exposes
// usable combined declaration bytes. Applied additionally identifies a writer
// invocation; neither result is runtime activation authority.
type PreparationCohortResult struct {
	Schema              string              `json:"schema"`
	Cohort              Reference           `json:"cohort"`
	Applied             bool                `json:"applied"`
	Complete            bool                `json:"complete"`
	DeclarationDocument string              `json:"declaration_document,omitempty"`
	DeclarationSha256   string              `json:"declaration_sha256,omitempty"`
	Roots               []string            `json:"roots"`
	Results             []PreparationResult `json:"results,omitempty"`
	RestartAuthorized   bool                `json:"restart_authorized"`
}

// Borrows the adapter synchronously; real descriptors and every root lease
// remain held through complete read-only admission, then close before return.
func CheckPreparationCohort(ctx context.Context, reference Reference, adapter PreparationAdapter) (PreparationCohortResult, error) {
	return prepareCohort(ctx, reference, adapter, defaultHost(), daemonScope, false, nil)
}

// Explicit fact injection cannot bypass physical journals or root membership.
func CheckPreparationCohortWithHost(ctx context.Context, reference Reference, adapter PreparationAdapter, host Host) (PreparationCohortResult, error) {
	return prepareCohort(ctx, reference, adapter, host, daemonScope, false, nil)
}

// Borrows the adapter; closes all roots on every return. A failure preserves
// completed roots and requires the exact original cohort for joined resumption.
func ApplyPreparationCohort(ctx context.Context, reference Reference, adapter PreparationAdapter) (PreparationCohortResult, error) {
	return prepareCohort(ctx, reference, adapter, defaultHost(), daemonScope, true, nil)
}

// Test facts remain instance-owned; publication and synchronous join are real.
func ApplyPreparationCohortWithHost(ctx context.Context, reference Reference, adapter PreparationAdapter, host Host) (PreparationCohortResult, error) {
	return prepareCohort(ctx, reference, adapter, host, daemonScope, true, nil)
}
