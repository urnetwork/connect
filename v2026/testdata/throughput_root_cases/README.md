# Throughput failure conditions

These are regression assertions, including failures that still need fixes.
They use explicit worker barriers, virtual time or exact timestamp sequences.
No failing case is skipped or converted into an expected-pass wrapper.
The model performance runner now invokes `NetworkQualityChanged` at each of its
three explicit path transitions; see
[the phase inventory](../../THROUGHPUTFIX-PR2.md#31-separate-notified-path-changes-from-core-pacing-validation).
These deterministic ownership and timing roots remain independent of the
end-to-end performance cells.

## Pacing discovery roots

The remaining static-path losses were the pace owner reading its own
release rate as the path's limit; see
[research plan section 34](../../THROUGHPUTFIX-PR2.md#34-the-pacer-measures-its-own-limit-discovery-and-held-pace).
`transfer_window_pacing_discovery_test.go` and
`transfer_window_pacing_discovery_state_test.go` hold the pure rate
contracts, the service-state root and the closed-loop cells; the
`TestWindowPacingCumulative*` contracts were re-pinned to the discovery floor
with queued and unqueued variants of the slow cases. The live correctness
selection runs all of them under race instrumentation.

## Test contract review

See [research plan section 30](../../THROUGHPUTFIX-PR2.md#30-audit-test-contracts-before-further-estimator-changes)
before interpreting a failed assertion as a production defect. Exact byte
ownership, ACK coverage and bounds remain invariants. Immediate estimator
responses, internal ring flags and throughput recovery deadlines describe
separate implementation or performance choices. A cold service estimate may
abstain when qualified cumulative delivery supplies the actual pacing rate;
assert that observable behavior rather than requiring every intermediate
estimate to be positive. Frozen original comparisons remain available.

`transfer_window_h1_policy_contract_test.go` checks the physical fixture's
configuration assertion independently of throughput. A valid configured arm
may have only service evidence or retain its learned window after both current
proofs expire. Constructor and actual destination sequence mode, scale and
instruments still have to agree. Eight roots also reject disabled settings,
missing destination owners and unknown arms. The live correctness selection
includes them; physical throughput/refusal/calibration gates remain separate.

The [receiver semantic audit](../../throughput-fix-2-results/service-receiver-semantic-audit-v1/semantic-audit.md)
keeps research fixtures separate from live package coverage. Oversized or
over-permission inputs are not production failure proof. Corrected cumulative
groups use two legal 6,000-byte messages and a bounded rolling opening. They
still expose a repeated difference between per-refill and batched estimator
reads, with exact credit and window limits preserved. The legal slowdown
comparison also reproduces excessive global pacing before the conservative
decrease correction. Per-lane passing rates do not certify aggregate utilization;
actual shared reservation cost and full-path performance remain separate gates.

The [shared cumulative consumer proof](../../throughput-fix-2-results/window-cumulative-shared-reserve-root-v3)
first isolated equal and unequal two-/four-lane utilization failures. The live
`transfer_window_shared_delivery_test.go` fixture now drives the same shared
reservation clock through real retained items, confirmed initial H1 writes,
once-only cumulative heads, repeated heads and late SACKs. One-, two- and
four-lane cases, idle siblings, independent destinations, carrier/permission
boundaries and statistics reads are part of the canonical correctness
selector. `transfer_window_service_delivery_test.go` owns the fixed-ring
endpoint, rebucketing, generation, freshness and memory bounds. The common
rate prices cold pacing only; positive serialization remains authoritative and
logical window sizing remains per sequence.

`ip_provider_pure_ack_admission_test.go` fills the pinned provider H1 lane's
two count-admission slots and produces an exact 52-byte pure TCP ACK through
the real per-flow compressor. It proves the ACK survives until one slot is
released under both window policies, newer cumulative progress is covered,
provider cancellation reclaims pool and admission ownership, and a public
callback still receives the shared worker's zero-wait refusal. The pre-fix
source loses the generated control in every full-admission arm.

`TestClientNetworkQualityFrequentCallbacksAreIdempotentWithStatistics` overlaps
128 concurrent native-style notifications with 32 public statistics readers.
One worker and the five-second quiet rule admit one generation, preserve valid
snapshots, then admit exactly one later generation. The local loopback is reset
but excluded from redundant peer fanout. Ten race repetitions complete without
warnings, recovered panics or race diagnostics.

## Caller cancellation ownership

`transfer_pack_context_owner_test.go` and
`transfer_forward_pack_context_owner_test.go` cover cancellation before and
during bounded admission, already accepted siblings, cancellation after
acceptance, and recreation after genuine shared-sequence closure. Constructor
hooks and `synctest.Wait` force the ordering without sockets or polling. Failed
admission retains caller ownership; accepted bytes retain sequence ownership.
The standalone matched comparison records six passes/18 failures before and
24 passes after under race, plus 24 adjacent passes. The canonical correctness
selector includes all eight roots. A separate frozen live-source comparison
repeats the same six passes/18 failures before, 24 passes after and 24 adjacent
passes, without races; the live sampler remains unchanged.

## Production cases

The 33 package tests added in this change were run three times under race on
commit `17780670` plus the tests. Thirteen pass all repetitions and twenty fail
all repetitions: 39 passes, 60 failures, no skips or race warnings. This source
excludes the uncommitted drain correction and every experimental receiver-service
estimator. The earlier 22-case run remains archived separately.

| Failure condition | Test file | Required controls |
| --- | --- | --- |
| A small compressed reply opens the next flight before the old tail, letting a window gap lower measured service | `transfer_window_compressed_flights_test.go` | Exact outstanding bytes and physical-tail ownership |
| Retaining a partial-feedback read poisons the rate saved by a successful drain | `transfer_window_successful_drain_feedback_test.go` | Receiver timing present/absent, no intermediate read, read-only statistics, identical successful drain and RTT proof |
| A first write waits beyond its original ACK lifetime | `transfer_window_initial_lifetime_test.go` | Recovery before/after lifetime; non-regenerable recovery remains retained |
| A retry, younger record, FIFO head, controlled drain or late timer wake hides expiry | `transfer_window_pacing_worker_lifetime_test.go` | Actual send worker, distinct older/younger deadlines, cancellation, live sibling and physical proof preserved |
| A drain waits after retry, carrier change or cancellation makes its physical delivery proof impossible | `transfer_window_pacing_unprovable_drain_test.go` | Invalidation before/during the wait, fresh-tail recovery, repeated notifications, unrelated probe ownership |
| A paused carrier reader compresses observed delivery and inflates the next burst | `transfer_window_pacing_carrier_buffer_test.go` | Fixed queues; 0/5/10/20/50 ms release delays; no drops/retries; unstalled and genuinely faster serializers |
| Short 10 Mb/s cells with 50 ms ACK compression lose service | `transfer_window_service_failure_cells_test.go` | Both one and eight flows; original warmup, serializer, reference and finite-queue gates |

Run these families from the repository root:

```sh
go test -race -count=3 -run '^TestWindowPacing(CompressedFlights|Initial|Carrier|Lifetime|ShortCompressedService|SuccessfulDrain|RetriedTail|InvalidatedTail|UnconfirmedCarrier|CanceledTail|FreshTail|AbortedDrain|RepeatedTail)'
```

The unchanged broad performance matrix remains a separate gate. The eleven
proved-RTT history tests committed in `17780670` already cover stale delivery,
delayed physical confirmation, read ordering, fixed bounds and other carriers.
The working drain correction passes its eleven liveness roots, but that
correction is excluded from the committed-source baseline above. Its wider
performance acceptance remains open.

## Lifetime and initial-feedback follow-up

The next cases extend the inventory when the lifetime candidate and broader
model exposed additional conditions. They remain ordinary assertions; a
performance failure is not converted into a passing reproduction wrapper.

| Failure condition | Executable coverage |
| --- | --- |
| A paced retry temporarily loses the original copy's ACK identity | `TestWindowPacingLifetimeRetryWaitKeepsAckIdentity` |
| Recovery order hides an earlier lifetime during an ordinary idle wait | `TestWindowPacingLifetimeIdleWaitKeepsOlderDeadline` |
| Queued head/SACK feedback protects an older message while a younger one waits | `TestWindowPacingLifetimePendingHeadPreservesYoungerWrite`, `TestWindowPacingLifetimePendingSackPreservesYoungerWrite`; also preserve the original RTT tag timestamp |
| Equal timer deadlines or a delayed admission allow expired data to dispatch | `TestWindowPacingLifetimeEqualPacingDeadlineExpiresFirst`, `TestWindowPacingLifetimeLateAdmissionCannotDispatch` |
| Route backpressure hides expiry after pacing has finished | `TestWindowPacingLifetimeFullRouteKeepsOriginalDeadline`, with `RouteRecoveryBeforeExpiry` and `RouteWakePreservesWriterBudget` controls |
| Cleanup of an expired younger message loses the older prefix's received ACK | `TestWindowPacingLifetimeAcknowledgedPrefixSurvivesYoungerExpiry` |
| Delivery arrives while an admitted retry is held | `TimelyHeadSurvivesDelayedAdmission`, `TimelySackSurvivesDelayedAdmission`, `LateHeadSuppressesDelayedRetry`, `LateSackSuppressesDelayedRetry` in `transfer_window_pacing_lifetime_adjacent_test.go` |
| Cancellation after pacing admission still publishes data | `TestWindowPacingLifetimeCancellationAfterAdmissionCannotDispatch`, `TestWindowPacingLifetimeStandaloneCancellationAfterAdmission` |
| An older SACK hides a newer valid compact-contract recovery request | `TestWindowPacingLifetimeNewerContractRequestPreservesRenewal`, with wrong-contract, full-proof and once-only renewal controls in `transfer_window_pacing_lifetime_contract_test.go` |
| The constrained H1 SDK sender loses capacity after its first feedback | `TestWindowPathSdkConstrainedInitialFeedback`, with `ConstrainedLaterFeedbackControl` and the original unforced `ConstrainedLongWindow` cell |
| A fully drained SDK opening prices one resumed ACK across its turnaround as service | `TestWindowPacingSdkInitialDrainedReplyDoesNotEstablishService`, with `InitialOutstandingFlightDiscoversSlowService` and `InitialFreshPairDiscoversSlowService` controls |

ACKs already received when the worker resumes retain the existing delivery-
before-expiry ordering. The late-ACK controls require suppressing the unissued
duplicate and preserving cumulative/selective results; they do not introduce
a new rule that converts received delivery into a timeout.

The SDK test forces the first ACK at 205 ms on the existing 400 ms RTT,
one-flow constrained-device cell. A 220 ms first ACK is its positive control.
The unchanged serializer, warmup, duration, finite bounds and 90% capacity gate
apply to both. The forced cell still has a mixed race result (two failures and
one pass); its first-ACK barrier alone is not a deterministic root proof. The
later control passes all three repetitions. The intermittent original cell
also fails on the source without the lifetime correction, so it is not
attributed to that correction. The
first-feedback barrier verifies the actual active lane mask, including the
original eight-lane duplex control.

The separate cold-service root is deterministic: a confirmed empty opening
followed by one resumed ACK yields 3,459 B/s and a 626 ms pacing delay. It fails
three times on independent lifetime-corrected sources, while both slow-service
controls pass three times each. All nine results are retained in
`throughput-fix-2-results/sdk-initial-service-root-before`.

The experimental retained-window policy has six additional deterministic tests
in `transfer_window_retained_test.go`. They exercise evidence-based growth,
adaptive pacing during slowdown, temporary hard bounds, target changes, missing
evidence and observational statistics. All 18 repeated assertions fail before
and pass after the isolated sizing change; the complete existing-root comparison
is retained in `throughput-fix-2-results/window-retention-policy-v1`. This is a
policy feasibility experiment with event plumbing deferred, not a replacement
for the sampler failure assertions or performance gates.

### Retained-window and sampler controls

| Condition | Executable coverage and evidence |
| --- | --- |
| A healthy serialization train cannot grow a window before cumulative history spans multiple RTTs | `TestWindowRetainedMeasuredServiceCanGrowConstrainedFlight`; a single-reply negative control and four actual-worker 400 ms cells live in `transfer_window_retained_growth_test.go` |
| An unrelated shared H1 rate grows an unknown, H3, P2P or mixed lane | `transfer_window_retained_service_scope_test.go`; actual route publications, H1 sibling and independent local-service controls |
| The target narrows a candidate but does not actually bind retained admission | Three `TestWindowRetainedTargetMetadata*` assertions; `window-retention-target-diagnostic` retains six before failures and 27 after passes |
| Campaign output labels qualified serialization growth as unsized | `TestWindowRetainedCampaignAcceptsServiceQualification`; cumulative-only, service-only, both and unqualified cases; three failures before and three passes after |
| A cold drained opening resumes before physical confirmation or complete logical accounting | `transfer_window_sdk_initial_service_order_test.go`; ACK-before-confirmation, delayed accounting, sibling-first feedback, unread valid pairs and late old-flight pairs |
| A successful drain is read between proof and resumed dispatch | `TestWindowPacingSuccessfulDrainReadBetweenProofAndDispatch`; receiver metadata and legacy cases |
| A drain hold masks real slower or faster service | `SuccessfulDrainTimeoutAcceptsSlowService`, `SuccessfulDrainAbandonmentAcceptsSlowService`, `SuccessfulDrainPartialDeliveryCanRaiseService`, `SuccessfulDrainConfirmedProbeAcceptsSlowService` |
| A later drain revives a retired measurement cycle | `TestWindowPacingSuccessfulDrainCannotRestoreRetiredCycle`; the first candidate fails three times and the corrected candidate passes |
| Rolling limited flights join their refill gaps into a false slow sample | `TestWindowPacingCompressedFlightsKeepMeasuredSerialization` and `TestWindowPacingLimitedFlightReorderedAccountingKeepsService` |
| Protecting limited flights masks genuine slower service | `TestWindowPacingLimitedFlightAcceptsContinuousSlowPair`, `TestWindowPacingLimitedFlightAcceptsQueuedSlowService`, `TestWindowPacingLimitedFlightRepeatedSlowRefillAdapts`; the repeated-refill control has no global drain or quality signal |
| Buffered carrier reads look faster after queue evidence clears or rotates out | `TestWindowPacingCarrierBufferedReadsAcrossSamplerBuckets`, `TestWindowPacingCarrierPeakStaysUnqualifiedAfterQueueClears`, `TestWindowPacingCarrierLateAccountingRetainsQueueProvenance` |
| Rejecting a queued peak prevents a later genuine increase | `TestWindowPacingCarrierFreshFastPairSupersedesRejectedPeak`, `TestWindowPacingCarrierSameBucketHoldEndsAtFreshTrain`, `TestWindowPacingCarrierSustainedQueuedIncreaseRaisesService`; original capacity-recovery controls remain unchanged |
| Delayed queued RTT overwrites a newer delivery bucket at the same ring index | `TestWindowPacingLateQueuedTimingCannotEvictRecentDelivery`; exact 64/65-bucket boundaries, legacy and receiver timing, with the valid RTT observation preserved |
| Dropping old timing markers also drops one that is still inside the service ring | `TestWindowPacingOldestRetainedQueuedTimingStillQualifiesDelivery`; the 63-bucket control still prevents a buffered peak after newer timing replaces the original tuples |
| Modest queue evidence lets a limited-flight refill gap reprice service | `TestWindowPacingLimitedFlightModestQueueKeepsSerialization`; actual physical tails keep the service occupied, while slower continuous, queued and repeated-refill controls still adapt |
| A new RTT admits old propagation silence into a fresh fast train | `TestWindowPacingFreshTrainRejectsPriorPropagationGap`; the below-allowance and sustained-slowdown tests preserve downward adaptation |
| A drained RTT probe empties the ring before the first queued byte accounting | `TestWindowPacingFirstQueuedTimingAfterEpochKeepsProvenance`; exact physical probe/tail identities, legacy/receiver timing and later timing eviction, with prompt-accounting and cold-empty controls |
| Timing-only queue markers revive an abandoned drain's reset | The unchanged `TestWindowPacingAbandonedDrainCannotResetLaterService` rejects the initial empty-ring fix; the corrected candidate requires actual delivery evidence |
| Statistics race with route-writer publication or retirement | `TestWindowStatsConcurrentWriterTeardown`, `TestWindowStatsConcurrentWriterPublication`; explicit access barriers reproduce both original races in three fresh processes |
| Synchronizing the writer blocks statistics behind external retirement | `TestWindowStatsWriterRetirementDoesNotBlockSnapshot`; a real retained route reference is released only after the statistics read completes |
| A cold pacer ignores qualified cumulative delivery, or borrows another carrier's rate | `transfer_window_cumulative_pacing_test.go`; one reply, tiny/stale/pre-permission history, hard bounds, positive service precedence, current H1-only policy and sibling/local ownership |
| An advertised ACK timer makes a refill gap complete a limited-flight cycle | `TestWindowPacingLimitedFlightAckPhaseDoesNotCompleteService`; actual receiver waiting, rolling physical tails and existing slow-service controls |
| Newer legacy timing borrows an older receiver delay | `transfer_window_service_feedback_delay_test.go`; both equal-timestamp orders, actual held feedback and delayed byte accounting |
| Changing receiver waits stretch a fast delivery interval into a false slow sample | `transfer_window_service_receiver_interval_test.go`; real coalescer, exact first H1 writes, overlapping flight, equal-wait slow service and corrected slower service |
| A qualified pair expires while cycle start and completion use different receiver-delay allowances | `transfer_window_service_refill_interval_test.go`; two siblings with callback-driven FIFO refills preserve physical overlap, while a 125 ms serializer must adapt to 10 kB/s |

The cold-start comparison passes all 24 repeated root/control executions after
12 passes/12 failures before. The subsequent drain comparison passes all 60
executions after 51 passes/9 failures before. These sampler-only comparisons
retain the preceding production sizing policy, so their 126-test affected
selection still exposes the separate short-compression window failure.
`sdk-initial-sampler-correction` and `successful-drain-sampler-correction`
retain the normalized evidence. The combined model is a separate acceptance
gate; no notification is used to make these roots pass.

The subsequent `service-qualification-correction` comparison repeats 14 roots
and controls three times: 21 passes/21 failures before, 42 passes after. The
154-test affected selection records 146 passes/8 failures before and 153
passes/1 failure after. The one remaining short-compression failure uses the
preceding sizing core. These results isolate one sampler-file change; they
do not establish combined throughput or validate later edits.

The separate `service-queue-horizon-correction` comparison records three
passes/three failures before and six passes after under race. Its affected
selection records 154 passes/two failures before and 155 passes/one failure
after. The remaining failure again belongs to the preceding sizing core.

`service-gap-qualification-correction` adds the traced gap conditions and
unchanged slow/fast controls: 24 passes/six failures before, 30 passes after.
The broader selection improves from 157 passes/three failures to 159 passes/one
older-core failure. Its combined six-gate model follow-through restores RTT
growth, but changing receiver windows and long SDK paths still fail.

`service-queue-epoch-correction` preserves the original, rejected and corrected
variants. Its four repeated roots record nine passes/three failures, nine
passes/three different failures, then 12 passes. The wider 163-test selection
ends at 162 passes/one older-core failure. Byte totals, physical drain proof
and the abandoned-drain invariant remain unchanged.

`window-writer-statistics-owner-v2` uses identical immediate-access barriers
in both variants. Each of three fresh before processes fails both race roots
and passes the retirement control; each after process passes all three. The
adjacent after selection adds 60 passes. The v1 archive retains the earlier
start barrier and its missed teardown reproduction.

The initial nine cumulative-pacing roots improve from 21 passes/six failures
to 27 passes. With carrier and sibling controls added, the separate scope
correction improves from 33 passes/three failures to 36 passes. These comparisons
use identical preceding samplers and preserve candidate arithmetic; actual
SDK throughput is recorded separately.

The ACK-phase correction improves 21 passes/three failures to 24 passes; the
mixed-feedback correction improves nine passes/three failures to 12 passes.
Both are repeated race comparisons. The new receiver-interval roots improve
three passes/six failures to nine passes on an isolated warm candidate, whose
actual six-gate throughput selection still has four passes/two failures.
`window-receiver-credit-attribution-v1` separately retains 18 passes/nine
failures before and 24 passes/three failures after: invalid identities, mixed
carriers/prefixes, sibling timing, delayed accounting, SACK replay and
nonpositive corrected clocks are covered; the cold short-train root still fails.
These candidate proofs do not establish full combined acceptance.

`service-receiver-refill-correction` records three passes/three failures before
and six passes after under race. `window-sdk-cold-receiver-qualification-v1`
retains the isolated cold proof: 30 passes/nine failures become 39 passes, with
93 adjacent race passes. Buffered readers and one cumulative reply cannot
create a cold capacity sample; a fully qualified short pair can. The combined
six-gate candidate then passes all targeted throughput tests, while endpoint
ordering and retained-summary consumers still need their own acceptance proofs.

The intermediate lifetime fixes are preserved in
`throughput-fix-2-results/lifetime-owner-evolution`: timestamp mutation and the
equal-deadline error each have a failing earlier candidate. The full v3
regression reports 3,203 passes, five service-root failures and 25 skips. Its
five failures map to the production table above. The separate v1 model has
23 passes/four failures, 782 service-reading JSON records and 36 compact model
summaries; SDK early exit prevents a claim that it measured the complete sweep.

Run the added lifetime family under race, and the affected SDK controls
separately:

```sh
go test -race -count=3 -run '^TestWindowPacingLifetime' .
go test -race -count=3 -run '^TestWindowPathSdkConstrained(InitialFeedback|LaterFeedbackControl)$' .
```

## Existing root and adjacent coverage

These earlier tests remain part of the acceptance gates; the new cases extend
their coverage rather than replacing their assertions.

| Failure family | Existing package test files |
| --- | --- |
| One cumulative head, oldest-first SACKs above it, head absorption, pacing and maximum response size | `transfer_ack_compression_test.go`, `transfer_ack_bounds_test.go` |
| Window mismatch, peer/configured/memory limits and fresh evidence after capacity changes | `transfer_window_mismatch_test.go`, `transfer_window_adjacent_test.go`, `transfer_window_clamped_fixture_test.go` |
| Per-service byte/time burst bounds, isolation, cancellation and delayed wakes | `transfer_window_pacing_test.go`, `transfer_window_pacing_wakeup_test.go`, `transfer_window_pacing_flight_test.go` |
| Bucket boundaries, zero hold, burst resets, late observations and source idle | `transfer_window_bucket_stats_test.go`, `transfer_window_burst_stats_test.go`, `transfer_window_host_feedback_test.go` |
| Physical retry timing, failed writes, carrier changes and original lifetimes | `transfer_window_retry_physical_time_test.go`, `transfer_window_retry_physical_adjacent_test.go` |
| Receiver timing identity, queued RTT versus unloaded RTT, delayed confirmation and long replies | `transfer_window_receiver_rtt_test.go`, `transfer_window_receiver_baseline_probe_test.go`, `transfer_window_receiver_baseline_order_test.go`, `transfer_window_pacing_paired_probe_worker_test.go` |
| Proved RTT changes invalidating old window history, with carrier and fixed-bound controls | `transfer_window_refill_proof_test.go`, `transfer_window_refill_adjacent_test.go`, `transfer_window_refill_order_test.go`, `transfer_window_refill_carrier_test.go` |

The broad model, TUN and server regression gates remain separate. The final
configured server campaign includes the database-backed connect and proxy
packages. Its clean source pairing, rejected recovered-panic run and corrected
diagnostic-clean rerun are recorded in the
[final report](../../THROUGHPUT-REPORT-PR2.md#final-configured-server-integration).

## Experimental receiver cases

The `receiver/` sources are a runnable research fixture, not production code.
The runner applies checked-in patches to pinned commit `272f95e5` in a new
directory, freezes the local glog dependency, and records source, binary and
log hashes. It leaves the working checkout untouched. Receiver tuples still
use a bounded test-only lookup; no new wire fields or overhead are claimed.

```sh
python3 tools/replay-throughput-fix-2-receiver-roots.py /tmp/receiver-roots --variant hybrid
```

Use a fresh output directory for each run. `--count` defaults to 3. The default
selection runs 36 roots and controls; `--run` can select a particular case or
an included affected-cell model. A nonzero exit means a real failing assertion.

| Variant | Pass/fail over three repetitions | Failure conditions preserved |
| --- | --- | --- |
| `early-handoff` | 84 / 24 | One receiver endpoint suppresses ordinary discovery; queue and forward-delay failures |
| `hybrid` | 87 / 21 | Client queue, upstream carrier queue, bucket placement, paced/buffered source clocks, forward-delay crossing and peak expiry |
| `queued-endpoints` | 90 / 18 | Client queue omission passes; upstream buffering, scalar queue controls and forward-delay failures remain |
| `sender-confirmed-rise` | 93 / 15 | Bucket-shifted buffering still inflates both clocks; cold discovery and genuine increases regress; forward-delay failures remain |

Every variant also checks physical retry-byte accounting, once-only logical
credit, ambiguous retry endpoints, shared-service and lane isolation, reordered
ACKs, source/window/controlled pauses, immediate wakeups, active siblings,
genuine slowdowns, the sender's 0.95 pacing factor, and reverse ACK compression.
Forward-delay cases require retaining known service until a fresh post-change
pair can distinguish unchanged capacity from a genuine decrease.

The two full model runs that pass all 27 tests and 818 readings do **not** pass
all these roots. The queued variant's model success does not establish that a
userspace read timestamp measures physical arrival under upstream buffering.

Complete outcomes are in `throughput-fix-2-results/root-condition-complete`,
the earlier `throughput-fix-2-results/root-condition-final`, and
`throughput-fix-2-results/receiver-root-replay`. Raw logs stay in the local run
directories. The first experimental replay export had a duplicate test
definition; that setup failure is excluded from runtime results.
