package connect

// net_extender_camouflage_stats.go — what the client's tcp camouflage dials
// came to, for status and telemetry (EXTENDER-UPDATE2 §3.2, §3.3, rollout
// gates).
//
// The extender counts how it classified each hello (extender.ExtenderCamouflage
// Stats); the client counts the other half, which only it sees: whether the
// camouflaged attempt won, whether the race fell to the legacy attempt (Phase A
// fallback), whether both failed (the Phase B loss a skewed or legacy-fronted
// client meets), and why a dial ran the legacy attempt alone. A camouflaged
// attempt the extender did not authenticate is told apart by how it failed: an
// extender that terminated it negotiated a protocol other than http/1.1 (Phase
// A), and one that spliced it presented a leaf the record key did not sign
// (Phase B).
//
// The counters are process-wide and cumulative, read independently like the
// extender's, and carry no address, key or name. Safe for concurrent use.

import (
	"errors"
	"sync/atomic"
)

// A camouflaged attempt the extender terminated without authenticating: it
// negotiated a protocol other than the authenticated path's http/1.1 (P4).
var errExtenderCamouflageUnauthenticated = errors.New("extender camouflage: the hello was not authenticated")

// A camouflaged attempt whose outer leaf the record key did not sign: the hello
// reached another site, which is the splice of a Phase B extender.
var errExtenderCamouflageLeafRejected = errors.New("extender camouflage: the outer leaf is not the extender's")

// ExtenderTcpCamouflageClientStats is the client's tcp camouflage activity,
// cumulative for the life of the process.
type ExtenderTcpCamouflageClientStats struct {
	// races the camouflaged attempt won
	CamouflageWonCount int64
	// races the legacy attempt won after the camouflaged attempt failed or was
	// still pending at the stagger (the Phase A fallback)
	LegacyWonCount int64
	// races where both attempts failed (the Phase B loss)
	BothFailedCount int64
	// races ended by a refusal or a limit, which either attempt would get
	RefusedCount int64
	// camouflaged attempts the extender terminated unauthenticated (Phase A)
	CamouflageUnauthenticatedCount int64
	// camouflaged attempts that met a leaf the record key did not sign (the
	// Phase B splice)
	CamouflageLeafRejectedCount int64
	// tcp dials that ran the legacy attempt alone, by reason: the config
	// carried no camouflage key, the fingerprint switch is not the Chrome
	// hello, or there was no borrowed name to front
	LegacyOnlyNoKeyCount       int64
	LegacyOnlyFingerprintCount int64
	LegacyOnlyNoFrontNameCount int64
}

var (
	extenderTcpCamouflageCamouflageWonCount         atomic.Int64
	extenderTcpCamouflageLegacyWonCount             atomic.Int64
	extenderTcpCamouflageBothFailedCount            atomic.Int64
	extenderTcpCamouflageRefusedCount               atomic.Int64
	extenderTcpCamouflageUnauthenticatedCount       atomic.Int64
	extenderTcpCamouflageLeafRejectedCount          atomic.Int64
	extenderTcpCamouflageLegacyOnlyNoKeyCount       atomic.Int64
	extenderTcpCamouflageLegacyOnlyFingerprintCount atomic.Int64
	extenderTcpCamouflageLegacyOnlyNoFrontNameCount atomic.Int64
)

// ExtenderTcpCamouflageClientStatsSnapshot reads the client counters. Each is
// read independently, so a sampler reads deltas, never a total that must agree
// across counters.
func ExtenderTcpCamouflageClientStatsSnapshot() ExtenderTcpCamouflageClientStats {
	return ExtenderTcpCamouflageClientStats{
		CamouflageWonCount:             extenderTcpCamouflageCamouflageWonCount.Load(),
		LegacyWonCount:                 extenderTcpCamouflageLegacyWonCount.Load(),
		BothFailedCount:                extenderTcpCamouflageBothFailedCount.Load(),
		RefusedCount:                   extenderTcpCamouflageRefusedCount.Load(),
		CamouflageUnauthenticatedCount: extenderTcpCamouflageUnauthenticatedCount.Load(),
		CamouflageLeafRejectedCount:    extenderTcpCamouflageLeafRejectedCount.Load(),
		LegacyOnlyNoKeyCount:           extenderTcpCamouflageLegacyOnlyNoKeyCount.Load(),
		LegacyOnlyFingerprintCount:     extenderTcpCamouflageLegacyOnlyFingerprintCount.Load(),
		LegacyOnlyNoFrontNameCount:     extenderTcpCamouflageLegacyOnlyNoFrontNameCount.Load(),
	}
}

// Counts how a camouflaged attempt failed, when the failure says.
func countExtenderCamouflageAttemptFailure(err error) {
	switch {
	case errors.Is(err, errExtenderCamouflageUnauthenticated):
		extenderTcpCamouflageUnauthenticatedCount.Add(1)
	case errors.Is(err, errExtenderCamouflageLeafRejected):
		extenderTcpCamouflageLeafRejectedCount.Add(1)
	}
}
