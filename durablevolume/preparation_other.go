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
