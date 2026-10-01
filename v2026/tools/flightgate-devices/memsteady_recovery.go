package main

import "fmt"

const (
	memsteadyRecoveryDelayMillis = int64(30_000)
	memsteadyRecoveryHoldMillis  = int64(20_000)
)

type memsteadyTransferBudgetAt struct {
	Millis int64
	Budget deviceTransferBudgetSample
}

// A connected phone can admit fresh background NAT flows after the measured
// burst has drained. Keep their final claims visible instead of confusing an
// arbitrary end timestamp with teardown of a disconnected, closed graph.
// This is aggregate recovery evidence, not proof of individual flow identity.
type memsteadyTransferRecovery struct {
	Policy                   string                            `json:"policy"`
	NotBeforeMillis          int64                             `json:"not_before_millis"`
	RootQuietMinBytes        int64                             `json:"root_quiet_min_used_bytes"`
	RootQuietMinMillis       int64                             `json:"root_quiet_min_millis"`
	NatQuietMinBytes         int64                             `json:"nat_quiet_min_used_bytes"`
	NatQuietMinMillis        int64                             `json:"nat_quiet_min_millis"`
	Witness                  *memsteadyTransferRecoveryWitness `json:"witness"`
	LateNatAdmissionAccepted bool                              `json:"late_nat_admission_accepted"`
}

type memsteadyTransferRecoveryWitness struct {
	StartMillis                   int64 `json:"start_millis"`
	EndMillis                     int64 `json:"end_millis"`
	Samples                       int   `json:"samples"`
	RootUsedBytes                 int64 `json:"root_used_bytes"`
	NatUsedBytes                  int64 `json:"nat_used_bytes"`
	RootReservedAfterWitnessBytes int64 `json:"root_reserved_after_witness_bytes"`
	RootReleasedAfterWitnessBytes int64 `json:"root_released_after_witness_bytes"`
	NatReservedAfterWitnessBytes  int64 `json:"nat_reserved_after_witness_bytes"`
	NatReleasedAfterWitnessBytes  int64 `json:"nat_released_after_witness_bytes"`
}

// verifyMemsteadyTransferRecovery only permits a late NAT/root increase after
// sustained, simultaneous recovery of the complete transfer hierarchy. Client,
// provider, Pack, carrier and temporary-client end gates remain independent.
// Neither a single dip, a pre-burst dip nor a monotonically retained burst is a
// witness. The strict below-baseline requirement also prevents a flat quiet
// series followed by one unexplained extra final byte from passing.
func verifyMemsteadyTransferRecovery(samples []memsteadyTransferBudgetAt, meta memsteadyMeta) (memsteadyTransferRecovery, error) {
	recovery := memsteadyTransferRecovery{
		Policy: "sustained-quiet-nat-recovery-v1", NotBeforeMillis: meta.QuietStart + memsteadyRecoveryDelayMillis,
	}
	var baseline, end, previous *memsteadyTransferBudgetAt
	for i := range samples {
		sample := &samples[i]
		if sample.Millis < meta.BaselineStart || sample.Millis > meta.EndMillis {
			continue
		}
		if previous != nil {
			for _, budget := range []struct {
				name     string
				previous transferByteBudgetSample
				current  transferByteBudgetSample
			}{
				{"transfer root", previous.Budget.Root, sample.Budget.Root},
				{"NAT", previous.Budget.Nat, sample.Budget.Nat},
			} {
				if budget.current.ReservedBytes < budget.previous.ReservedBytes || budget.current.ReleasedBytes < budget.previous.ReleasedBytes {
					return recovery, fmt.Errorf("%s cumulative reserve/release counters decreased at %d", budget.name, sample.Millis)
				}
			}
		}
		previous = sample
		if sample.Millis < meta.StartMillis {
			baseline = sample
		}
		if sample.Millis >= meta.QuietStart {
			end = sample
			if recovery.RootQuietMinMillis == 0 || sample.Budget.Root.UsedBytes < recovery.RootQuietMinBytes {
				recovery.RootQuietMinBytes, recovery.RootQuietMinMillis = sample.Budget.Root.UsedBytes, sample.Millis
			}
			if recovery.NatQuietMinMillis == 0 || sample.Budget.Nat.UsedBytes < recovery.NatQuietMinBytes {
				recovery.NatQuietMinBytes, recovery.NatQuietMinMillis = sample.Budget.Nat.UsedBytes, sample.Millis
			}
		}
	}
	if baseline == nil || end == nil {
		return recovery, fmt.Errorf("missing transfer-budget baseline or quiet recovery evidence")
	}
	var start, last int64
	count := 0
	var witnessBudget deviceTransferBudgetSample
	for _, sample := range samples {
		if sample.Millis < recovery.NotBeforeMillis || sample.Millis > meta.EndMillis {
			continue
		}
		budget, base := sample.Budget, baseline.Budget
		if budget.Root.UsedBytes >= base.Root.UsedBytes || budget.Nat.UsedBytes >= base.Nat.UsedBytes ||
			budget.Client.UsedBytes > base.Client.UsedBytes || budget.Provider.UsedBytes > base.Provider.UsedBytes ||
			budget.Pack.UsedBytes > base.Pack.UsedBytes || budget.Pins.UsedBytes != base.Pins.UsedBytes {
			start, last, count = 0, 0, 0
			continue
		}
		if start == 0 || sample.Millis-last > 20_000 {
			start, count = sample.Millis, 0
		}
		last = sample.Millis
		count++
		if sample.Millis-start >= memsteadyRecoveryHoldMillis {
			recovery.Witness = &memsteadyTransferRecoveryWitness{
				StartMillis: start, EndMillis: sample.Millis, Samples: count,
				RootUsedBytes: budget.Root.UsedBytes, NatUsedBytes: budget.Nat.UsedBytes,
			}
			witnessBudget = budget
		}
	}
	if recovery.Witness != nil {
		witness := recovery.Witness
		witness.RootReservedAfterWitnessBytes = end.Budget.Root.ReservedBytes - witnessBudget.Root.ReservedBytes
		witness.RootReleasedAfterWitnessBytes = end.Budget.Root.ReleasedBytes - witnessBudget.Root.ReleasedBytes
		witness.NatReservedAfterWitnessBytes = end.Budget.Nat.ReservedBytes - witnessBudget.Nat.ReservedBytes
		witness.NatReleasedAfterWitnessBytes = end.Budget.Nat.ReleasedBytes - witnessBudget.Nat.ReleasedBytes
		// Only NAT growth can justify the root exception. An unrelated retained
		// root claim must not ride along with later background NAT activity.
		natExcess := end.Budget.Nat.UsedBytes - baseline.Budget.Nat.UsedBytes
		rootExcess := end.Budget.Root.UsedBytes - baseline.Budget.Root.UsedBytes
		recovery.LateNatAdmissionAccepted = natExcess > 0 && rootExcess <= natExcess &&
			witness.NatReservedAfterWitnessBytes > 0 &&
			witness.RootReservedAfterWitnessBytes >= witness.NatReservedAfterWitnessBytes &&
			witness.RootReservedAfterWitnessBytes-witness.RootReleasedAfterWitnessBytes == end.Budget.Root.UsedBytes-witness.RootUsedBytes &&
			witness.NatReservedAfterWitnessBytes-witness.NatReleasedAfterWitnessBytes == end.Budget.Nat.UsedBytes-witness.NatUsedBytes
	}
	return recovery, nil
}
