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
| A deliberate drain's idle gap replaced established service with one probe's apparent rate | [pacing-service-epoch-before.txt](pacing-service-epoch-before.txt) | `TestWindowPacingDrainedProbeHoldsServiceUntilFreshEvidence`, with direct and covering ACKs and fresh slower service |
| A probe ACK arrived before the physical writer confirmed H1 | [pacing-service-epoch-before.txt](pacing-service-epoch-before.txt) | `TestWindowPacingProbeAckBeforeWriteConfirmationHoldsService`, including unsuccessful confirmation |
| A budget-admission test mistook a reclaimed reservation for over-admission | [webrtc-admission-before.txt](webrtc-admission-before.txt) | `TestWebRtcNetworkPeerAdmissionWaitsOnDedicatedBudget`, with a physical-teardown barrier and exact budget accounting |
| Natural drains discarded faster serialization and reused a one-shot reset | [committed-b5-probes.txt](../controlled-epoch-final/failure-before/committed-b5-probes.txt) | `TestWindowPacingNaturalProbePreservesSerializationEvidence`, `TestWindowPacingAbandonedDrainCannotResetLaterService`, and the extended controlled-drain hold test |
| An established 64 KiB sender could not discover a later 1→10 Mb/s increase | [committed-b5-settled-capacity.txt](../controlled-epoch-final/failure-before/committed-b5-settled-capacity.txt) | `TestWindowPathServiceSettledLargeMessageCapacityChanges`, preserving the original settling allowance and throughput gate |
| A proposed deadline clear discarded a successfully drained pause | [intermediate-late-dispatch.txt](../controlled-epoch-final/failure-before/intermediate-late-dispatch.txt) | `TestWindowPacingLateDispatchKeepsSuccessfulDrainEpoch` |
| A proposed head-cancellation clear discarded its successor's inherited pause | [intermediate-canceled-head.txt](../controlled-epoch-final/failure-before/intermediate-canceled-head.txt) | `TestWindowPacingCanceledHeadTransfersControlledDrain`, with a third sequence owning the outstanding tail |
| RTT-driven bucket resizing erased the checkpoints proving a faster service | [committed-6a-rtt-pair.txt](../rtt-resize-evidence/failure-before/committed-6a-rtt-pair.txt) and [committed-6a-capacity-recovery.txt](../rtt-resize-evidence/failure-before/committed-6a-capacity-recovery.txt) | `TestWindowPacingShorterRoundTripKeepsFasterServiceEvidence` and `TestWindowPathServiceCapacityIncreaseRecovery` |
| A proposed rebucketing correction created overlapping sums for a reordered ACK | [intermediate-reordering.txt](../rtt-resize-evidence/failure-before/intermediate-reordering.txt) | `TestWindowPacingResizedSamplesKeepFirstBytesAndReordering`, with growth, retention and epoch controls alongside it |
| A small replay across source idle replaced established service and delayed resumed bulk traffic | [root-boundaries-final.txt](../source-idle-evidence/failure-before/root-boundaries-final.txt) | `TestTcpReturnReplayPreservesPacingServiceAcrossFeedbackIdle`, using the real recovery worker, plus five adjacent failing cases for confirmation, cancellation, real Pack admission and shared tails |

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

The separate [inner-TCP replay diagnostic](../host-feedback-controlled-epoch/replay-root.json)
preserves the original synthetic [test source](../host-feedback-controlled-epoch/replay-root-test.go.txt)
and three identical failed outcomes. The correction and nine normal regression
tests now live in the Go source. [Source-idle evidence](../source-idle-evidence/provenance.json)
records six before failures repeated three times and 276 focused race passes
afterwards; [full correctness](../correctness-source-idle/provenance.json) passes
169 tests. This root is distinct from bucket resizing and the still-open slow
shared-service and SDK bidirectional performance failures.
