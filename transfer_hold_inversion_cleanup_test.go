// A policy comparison must restore the caller's starting regime after its
// cleanup, including when that caller explicitly selected constant sizing.
package connect

import (
	"testing"
	"testing/synctest"
)

// The inner test's cleanup has completed before the outer policy is read.
// Reading DefaultWindowSizing inside that cleanup would preserve its final
// delivery policy instead of restoring the constant policy it entered with.
func TestReceiveHoldBudgetComparisonRestoresWindowSizing(t *testing.T) {
	original := DefaultWindowSizing()
	t.Cleanup(func() { SetWindowSizing(original) })
	SetWindowSizing(WindowSizingConstant)
	synctest.Test(t, TestTheReceiveHoldAndThePeerWindowCrossAtFourFifthsOfTheSendersBudget)
	if policy := DefaultWindowSizing(); policy != WindowSizingConstant {
		t.Fatalf("budget comparison leaked policy %v, want original constant policy %v", policy, WindowSizingConstant)
	}
}
