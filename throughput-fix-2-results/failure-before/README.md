# Deterministic failure-before evidence

These files retain failed regression tests before their corresponding correction
was applied, or with that correction temporarily reverted. The working source
contains the corrected implementation. Test names, rather than historical line
numbers in these logs, identify the current reproductions.

The new pacing cases use explicit byte/time observations, virtual time through
`testing/synctest`, or a channel barrier at the production handoff. They do not
depend on a host being idle or on a race happening by chance.

| Failure | Retained output | Current regression |
|---|---|---|
| Sparse compressed heads measured twice the actual service | [pacing-root-before.txt](pacing-root-before.txt) | `TestWindowPacingBackloggedSparseHeadsKeepTheirTime` |
| A later producer overtook an older delayed reservation | [pacing-root-before.txt](pacing-root-before.txt) | `TestWindowPacingWaitingWritersKeepReservationOrder` |
| Continuous flight retained a stale propagation floor | [pacing-continuous-rtt-before.txt](pacing-continuous-rtt-before.txt) | `TestWindowPacingContinuousFlightRefreshesChangedRoundTrip` |
| An ACK credited an admitted write that had not begun | [pacing-handoff-production-before.txt](pacing-handoff-production-before.txt) | `TestWindowPacingAdmissionCannotCreditAnUnbegunWrite`, through the real send method and ACK coalescer |
| Reordered samples were discarded within the current burst | [burst-ring-reorder-before.txt](burst-ring-reorder-before.txt) | `TestWindowPacingBurstStatsAcceptRetainedCurrentBurst` |
| A later burst's reset rejected earlier arrivals with the same burst identity | [burst-reset-reorder-before.txt](burst-reset-reorder-before.txt) | `TestWindowPacingBurstStatsAcceptRetainedAfterReset`, with same-bucket and preceding-bucket arrivals |
| One late physical release acquired two nominal burst identities | [pacing-dispatch-epoch-before.txt](pacing-dispatch-epoch-before.txt) | `TestWindowPacingBurstEpochFollowsActualDispatch` |
| An estimate change erased already spent burst bytes | [pacing-estimate-change-before.txt](pacing-estimate-change-before.txt) | `TestWindowPacingChangedEstimateRetainsActualBurstCharge` |
| A passed nominal deadline forgave a newly released physical burst | [pacing-late-decrease-before.txt](pacing-late-decrease-before.txt) | `TestWindowPacingDecreasedEstimateCannotForgiveALateRelease` |
| A historical unpaced storm control inherited delivery sizing and H1 pacing | [relay-control-before.txt](relay-control-before.txt) | `TestRelayInflationUsesConstantSendWindow`, both endpoints of all three historical arms |

The surrounding tests cover canceled/reused waiters, missing and selective tail
ACKs, carrier changes, bounded drain deadlines, duration overflow, startup probe
boundaries, partial buckets and zero-order hold. The independent ACK compression
tests retain the one-head, oldest-first SACK, absorption, pacing and maximum
serialized response-size requirements.

Run the normal race-enabled correctness selection from the repository root:

```sh
tools/throughput-fix-2.sh correctness /tmp/window-correctness
```

The paired performance model is a separate gate. A reproduction passing does
not excuse a failing performance cell; all measured comparisons and exclusions
belong in that run's ledger.
