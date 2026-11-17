// The current unanswered candidate cohort, rather than an older identical ISN,
// must own recovery after a real local retention failure.
package connect

import "testing"

// Reuse the exact real-Transfer ABA/capacity fixture, but retry final A before
// winner promotion can discard its stale old-A retained claim.
func TestColdSynCandidateUnwrittenAbaRetriesBeforePromotion(t *testing.T) {
	GetMessagePoolAggregateStats()
	assertMessagePoolOwnership(t)
	testTcpGroupRecoveryAmbiguousRaceAba(t, false, false, true, true)
}
