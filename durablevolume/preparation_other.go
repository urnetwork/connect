//go:build !linux

// No non-Linux path silently substitutes weaker physical custody semantics.
package durablevolume

import "context"

func planPreparation(context.Context, Reference, PreparationAdapter, Host, ownerScope) (PreparationPlan, error) {
	return PreparationPlan{}, ErrUnsupported
}
func applyPreparation(context.Context, Reference, PreparationAdapter, Host, ownerScope, *preparationHooks) (PreparationResult, error) {
	return PreparationResult{}, ErrUnsupported
}
