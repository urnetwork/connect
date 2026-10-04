//go:build !linux

// No non-Linux path silently substitutes weaker physical custody semantics.
package durablevolume

import "context"

// Unsupported hosts cannot produce an apparently applicable custody plan.
func planPreparation(context.Context, Reference, PreparationAdapter, Host, ownerScope) (PreparationPlan, error) {
	return PreparationPlan{}, ErrUnsupported
}

// The Linux physical profile has no implicit portable publication fallback.
func applyPreparation(context.Context, Reference, PreparationAdapter, Host, ownerScope, *preparationHooks) (PreparationResult, error) {
	return PreparationResult{}, ErrUnsupported
}

// Cross-root admission never substitutes weaker portable custody.
func prepareCohort(context.Context, Reference, PreparationAdapter, Host, ownerScope, bool, *preparationHooks) (PreparationCohortResult, error) {
	return PreparationCohortResult{}, ErrUnsupported
}

// Artifact preparation retains the same unsupported-platform refusal.
func RetainPreparationPlan(context.Context, PreparationPlan) (Reference, error) {
	return Reference{}, ErrUnsupported
}

// No portable staging artifact can imply qualified target preparation.
func RetainPreparationRequest(context.Context, PreparationRequest) (Reference, error) {
	return Reference{}, ErrUnsupported
}

// Cohort artifacts cannot imply weaker portable runtime admission.
func RetainPreparationCohort(context.Context, PreparationCohort, string) (Reference, error) {
	return Reference{}, ErrUnsupported
}
