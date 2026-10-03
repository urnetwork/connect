# Window follow-up evidence

Read [the results report](../THROUGHPUT-PR2-RESULTS.md) and
[the peer review/research plan](../THROUGHPUTFIX-PR2.md) for interpretation.
Rates in model rows are virtual-time payload Mb/s. TCP rows are host-clock
payload Mb/s. Server output uses MiB/s; these units are not interchangeable.

## Validation and root-cause evidence

- [Final core regression and runner contracts](core-regression-burst-hold-final-v1):
  3,345 root passes, 25 existing skips, zero failures on the validated held-pacing
  source; five snapshot and five scoped-environment contract tests pass.
- [Final scoped server regression](server-scoped-configure-final-v1):
  35 connect race passes and 28 proxy nonrace passes. Both binaries use cgo
  disabled, retain the reviewed configuration exports and record that the
  unrelated database-service preflight did not run. Integration modes are unchanged.
- [Permitted reverse burst and held pacing](service-pacing-burst-hold-v1):
  the matched component policy changes 12 race passes/three failures to 15
  passes. Both SDK arms pass five repetitions, so no performance uplift is
  inferred. The fresh candidate core model passes 36 definitions/846 readings
  and combined correctness passes 512 race definitions. All original outcomes,
  source/build hashes and the prior intermittent failure remain available.
- [Live prefix timing and short duplex comparison](service-receiver-prefix-live-sdk-v1):
  an exact one-file production pair changes 45 race passes/15 failures to
  60 passes. The repeated SDK pair improves from three failing definitions to
  two passes/one failure; the remaining non-providing upload is 855.296 versus
  957.2352 Mb/s reference. Both complete pinned arms and the separately labeled
  exploratory six-pass after run are preserved. The later burst-hold candidate
  passes current core validation without erasing this earlier failure.
- [Live-tree model-core baseline](model-core-live-baseline-v1): 23 passes,
  four failures and 812 readings on the working tree before the pacing fix.
  Nine SDK profile cells, the device H1 400 ms constrained window, the
  device H1 bidirectional reverse direction and the 2 MiB to 64 KiB
  permission change at 50 ms all lose capacity to the pace owner following
  its own release rate; see research plan section 34.
- [Pacing discovery floor and held pace](pacing-discovery-root-v1): pure
  rate contracts, service-state roots and closed-loop cells before and after
  the two controller rules, plus the re-pinned cumulative contracts. The
  device sender's 7 MB window is released within eight residences at 100 ms
  and 400 ms; the permission-shrink cell recovers; both capacity controls
  keep ending discovery on their own. The race correctness selection ends at
  486 passes with the four pre-existing receiver-wait estimator roots open.
- [Quality-change phase selection](model-core-deferral-v1): the `model-core`
  runner retains 27 tests and defers exactly three explicit propagation-switch
  performance tests until `NetworkQualityChanged` is called in those tests.
  The full `model` mode remains unchanged. This is a syntax/selection audit,
  not a model execution or passing performance result.
- [Shared cumulative credit attribution](window-cumulative-shared-credit-root-v4):
  45 passes/12 failures reproduce the four shared-consumer utilization defects
  through actual queued send items, cumulative heads, duplicates and late SACKs.
  Training remains prescribed; the consumer executes real reservation waits.
- [Shared-delivery oracle correction](window-cumulative-shared-delivery-oracle-v1):
  retains 57 passes/15 failures before and 69 passes/three failures after.
  The new missing-history assertion wrongly required resetting an already
  learned window. The corrected test preserves that window without changing
  production or any utilization threshold.
- [Common service delivery candidate](window-cumulative-shared-delivery-cold-v2):
  the corrected matched comparison changes from 57 passes/15 failures to
  72 race passes. Equal and unequal two-/four-lane consumers release all
  requested bytes within their original deadlines, with single-lane,
  early-return, carrier, permission, stale and observational controls intact.
  This is isolated prescribed-training/component evidence, not full throughput.
- [Cumulative prefix timing audit](service-receiver-prefix-timing-v1/contract-audit.md):
  a legal reordered head reprices a 0.2 MB/s prefix at 16 MB/s. The correction
  fixes that root while two older immediate exact-rate assertions fail because
  the head's wait does not locate the entire prefix. All original outcomes and
  test definitions remain available; eventual pacing is still required.
- [Warm common-delivery proof](service-shared-aggregate-warm-v1): 24 passes/three
  failures become 27 race passes for bounded prescribed input. This does not
  establish growth when the pacer itself controls the offered workload.
- [Common-ring boundary guards](service-shared-aggregate-boundaries-v1):
  15 passes/three failures become 18 passes after excluding offers from before
  a pending drain; all 27 warm controls still pass. Cold raw history retains
  its separate qualification rules.
- [Relayed prefix timing on one local H1 hop](service-receiver-prefix-local-hop-v2):
  12 passes/six failures become 18 passes. A known local route does not prove
  destination ingress order across a relay.
- [Accepted cumulative-prefix test contracts](service-receiver-prefix-contract-correction-v3):
  the two underdetermined exact-rate assertions retain byte, drain and raw-clock
  bounds; a new single-head control retains exact serialization coverage.
  Original definitions, exact diff and 60 passing race checks are preserved.
- [Common-delivery pacing maximum](window-cumulative-shared-warm-pacing-v2):
  both arms pass 183 race checks, but both fail the unchanged SDK Long group
  at fixed 100 ms and 400 ms RTT. Taking the larger service/common rate does
  not fix startup. All 40 model readings and 370 outcomes are retained.
- [Common-ring CPU and allocation cost](window-cumulative-shared-delivery-cpu-v1):
  all 54 benchmark readings report zero allocations. The ring adds 3,608 bytes
  per service; median real-head processing rises from 1,068 to 1,444 ns and
  estimation from 1,465 to 1,838 ns under concurrent load. These are diagnostic
  costs of the isolated candidate, not adoption or throughput acceptance.
- [Joined common-delivery and prefix SDK check](window-cumulative-shared-prefix-sdk-v1):
  87 race passes/six failures retain the two disputed exact SDK assertions.
  The unchanged short SDK selection passes; the long one fails, including a
  51.607/131.046 Mb/s comparison. The complete twenty readings are retained.
- [Static long-path recovery](model-static-long-recovery-v1): two new controls
  pass with actual senders, ACK workers and pacing from a 512 KiB opening.
  Fixed 400 ms and 1.2-second paths reach their paired references without drops
  or timeout resends after predeclared long warmups. This establishes settled
  performance for those cells, not fast SDK startup or full-model acceptance.
- [Lifetime ownership evolution](lifetime-owner-evolution) and
  [final independent comparison](lifetime-owner-final): retained intermediate
  failures and the independently committed correction's 111 passing race
  executions, excluding the separate working drain change.
- [Lifetime v7 correctness](correctness-lifetime-owner-v7) and
  [root regression](regression-lifetime-owner-v7): 377 race passes/six failures
  and 3,219 non-race passes/six failures/25 skips. The six service assertions
  remain failures; these snapshots contain no retained-window policy.
- [SDK first-feedback phase isolation](sdk-initial-feedback-isolation) and
  [affected cells](sdk-initial-feedback-roots): the forced 205 ms first reply
  still has a mixed race outcome; the 220 ms control passes. These are not
  deterministic sampler-root proof.
- [SDK cold-service root before correction](sdk-initial-service-root-before):
  three identical false-rate/pacing-delay failures and six slow-service control
  passes on independent lifetime-corrected sources.
- [Retained-window policy v1](window-retention-policy-v1): all 614 outcomes from
  the six new policy assertions and unchanged existing-root comparisons.
  Superseded shrink/jump assertions and remaining sampler failures stay visible.
- [Target-bound diagnostic correction](window-retention-target-diagnostic):
  six deterministic before failures; all 27 policy assertions pass afterward.
- [Measured-service growth root](window-retention-service-growth): a healthy
  serialized opening cannot fill a multi-RTT cumulative history; the candidate
  passes all 33 focused assertions after three identical before failures.
  Full throughput and carrier-attribution validation remain separate gates.
- [Original full retained-window comparison](window-retention-model-v1):
  25 passes/5 failures before versus 20 passes/10 failures after. Retention
  alone underfills long-RTT paths; this candidate is not accepted as a full fix.
- [SDK cold-start sampler correction](sdk-initial-sampler-correction):
  12 passes/12 failures before versus 24 passes after across the eight repeated
  roots and ordering controls, with stable copied sources under race.
- [Successful-drain sampler correction](successful-drain-sampler-correction):
  51 passes/9 failures before versus 60 passes after. Deadline, abandonment,
  fresh slow service, faster prefixes and retired-cycle controls are retained.
- [Service qualification correction](service-qualification-correction):
  21 passes/21 failures before versus 42 focused race passes after; the wider
  selection improves from 146 passes/8 failures to 153 passes/1 failure.
  Rolling-flight gaps, buffered carrier peaks and nine adjacent cases are
  covered. The remaining short-compression failure uses the preceding sizing
  core; this is an isolated sampler comparison, not combined acceptance.
- [Rejected qualification candidate](service-qualification-v2-rejected):
  36 focused passes masked two new capacity-recovery regressions in the broader
  selection. All four before/after runs remain available; the later correction
  preserves and passes those original recovery controls.
- [Queued RTT ring boundary](service-queue-horizon-correction): three passes/
  three failures become six race passes. Exact retired-slot edges cannot erase
  newer delivery; the oldest retained queue marker still qualifies its bytes.
  The broader selection improves from 154 passes/two failures to 155 passes/one
  failure on the older sizing core.
- [Fresh-train and limited-flight gaps](service-gap-qualification-correction):
  24 passes/six failures become 30 race passes. The broader isolated selection
  improves from 157 passes/three failures to 159 passes/one older-core failure.
  Genuine slow and fast adaptation controls remain unchanged.
- [Queue provenance after an empty-ring reset](service-queue-epoch-correction):
  nine passes/three failures become 12 passes. The rejected first candidate
  fixes the new root but fails the unchanged abandoned-drain control; both
  remain visible. The corrected 163-test selection has 162 passes and the one
  known older-core short-compression failure.
- [ACK phase at limited-flight completion](service-flight-phase-correction):
  21 passes/three failures become 24 race passes. The wider selection has
  163 passes and one older-core short-compression failure. An advertised
  compression timeout must not replace the actual wait in paired feedback.
- [Mixed receiver and legacy timing](service-feedback-delay-correction):
  nine passes/three failures become 12 race passes. Newer legacy observations,
  equal timestamps and delayed accounting cannot borrow an older receiver wait.
  The wider selection ends at 167 passes/one older-core failure.
- [Exact timing-credit attribution](window-receiver-credit-attribution-v1):
  18 passes/nine failures become 24 passes/three failures under race. Invalid
  tags, retries, other carriers, mixed prefixes, delayed accounting, SACK replay
  and nonpositive corrected intervals are covered. The remaining failure is
  the repeated cold SDK short-train root; the candidate remains isolated.
- [Warm receiver-interval correction](service-receiver-interval-correction):
  three passes/six failures become nine race passes. Changed waits no longer
  turn 12.5 MB/s into 0.25 MB/s; equal-wait and corrected-slower controls pass.
  The wider selection ends at 170 passes/one older-core failure, while the
  actual combined throughput checkpoint still has two failures.
- [Receiver wait at rolling-refill cycle start](service-receiver-refill-correction):
  three passes/three failures become six race passes. A healthy pair aging out
  cannot turn alternating sibling refills into slow service; the genuinely
  slow serializer still changes the rate. The wider selection ends at
  172 passes/one older-core failure.
- [Cold receiver-pair qualification](window-sdk-cold-receiver-qualification-v1):
  30 passes/nine failures become 39 race passes, with 93 adjacent passes.
  Cold short pairs can establish service while buffered readers, one reply
  and legacy feedback retain their guards. The unchanged SDK short/long subset
  passes both tests, with 20 paired readings; this throughput follow-through is
  after-only, not an isolated before/after model comparison.
- [Receiver-clock consumers and endpoint bounds](service-receiver-consumers-bounds-v1):
  ten repeated roots improve from 12 passes/18 failures to 30 race passes;
  the affected selection improves from 194 passes/six failures to 200 passes.
  Both arms use the same refined endpoint bounds. The after-only six-gate
  nonrace model also passes. An initial 27-execution selector is retained;
  four earlier mixed-feedback definitions were absent from both copied sources.
  Their supplement is below. An integration audit later found an omitted
  cumulative pacing fallback, so this source is not eligible for adoption.
- [Mixed-feedback supplement](service-receiver-mixed-supplement-v1): all four
  previously omitted roots pass three times in each arm, 24 race passes total.
  The original 200-outcome selections remain unchanged.
- [Cumulative pacing restoration](service-receiver-cumulative-restoration-v1):
  four earlier root failures reproduce three times; restoring the accidentally
  omitted helper changes 24 passes/12 failures to 36 race passes. All twelve
  assertions remain unchanged. The restored normal six-gate run has five
  passes and one RTT-growth failure, which remains open.
- [Shared cumulative pacing root](window-cumulative-shared-reserve-root-v3):
  45 race passes/12 failures. Equal and unequal two-/four-lane release targets
  each fail three times while single-lane, idle/stale, independent-service and
  all twelve original controls pass. Training feedback is prescribed within
  legal frame/window bounds; the consumer uses actual reservations and waits.
  The [first precondition error](window-cumulative-shared-reserve-precondition-v1)
  and [partial table coverage](window-cumulative-shared-reserve-root-v2) remain
  separate. No production fix or closed-loop throughput result is claimed.
- [Corrected endpoint bounds](window-receiver-ordering-bounds-v3): both delayed
  interior publication orders fail before refinement; 21 passes/three failures
  become 24 race passes, with 132 adjacent passes. Invalid-interval rejection,
  first-byte attribution and recovery controls remain intact.
- [Interleaved sustained slowdown](window-receiver-interleaved-slowdown-root-v1):
  the paired case fails all three repetitions, holding 12.5 MB/s while a
  continuously queued serializer supplies 4 MB/s. All 64 paired intervals
  become invalid. Its credit/mechanism checks pass, as do six legacy and mixed
  controls, but later review finds oversized and over-window input. Treat this
  as producer-mechanism evidence. The expanded semantic audit below retains
  separate legal rolling-flight proof; this original run claims no fix.
- [Bounded slowdown correction](service-receiver-invalid-slowdown-v1): corrected
  matched roots improve from 18 passes/three failures to 21 race passes; complete
  scoped correctness passes 507 checks. Invalid endpoint order permits only a
  conservative decrease from sustained queued evidence. The original invalid
  fast-control fixture is separately retained; throughput acceptance remains open.
- [Complete receiver-clock scoped baseline](correctness-receiver-service-v1):
  502 race passes/one failure, the original interleaved-slowdown root. Restored
  cumulative fallback and every earlier test are included on the copied source.
- [Complete receiver-clock regression baseline](regression-receiver-service-v1):
  3,334 nonrace passes/one known interleaved-slowdown failure/25 skips. Every
  top-level outcome is retained; the later slowdown fix has separate scoped
  proof and still needs combined performance acceptance.
- [Receiver test-contract audit](receiver-test-contract-audit-v1): detailed
  review plus the frozen sibling test source and all fifteen race outcomes.
  Six controls pass; nine immediate exact-rate assertions fail. Their ownership
  preconditions pass, and alternative deferred/smoothed responses require
  sustained-convergence tests before these are labeled correctness defects.
- [Expanded receiver semantic audit](service-receiver-semantic-audit-v1): retains
  invalid input fixtures, corrected legal controls and the narrower repeated
  batched-read failure. Per-lane pacing passes are distinguished from aggregate
  shared-service utilization. A passing finite-relay diagnostic does not close
  the original full-model failure.
- [RTT measurement-window policy review](model-rtt-measurement-policy-review-v1):
  offline reaggregation of five existing paired measurements, preserving the
  original one-second gate. One borderline run changes classification with
  longer bins; severe losses fail every aggregation. No workload, threshold
  or production change is implied.
- [ACK-owner fixture audit v2](physical-h1-ack-owner-fixture-audit-v2): preserves
  the shared-cancel setup mistake and late-installed-hook race. These are
  fixture defects, not reasons for additional production changes.
- [ACK-owner policy v3](physical-h1-ack-owner-policy-v3): corrected fixtures give
  24 passes/27 failures before and 51 root plus 110 adjacent race passes after,
  without warnings. Split patches keep hard caller isolation separate from
  the optional dedicated-worker ACK retention policy.
- [Standalone caller isolation](caller-cancellation-isolation-v2): eight roots
  repeated three times improve from six passes/18 failures to 24 race passes,
  with 24 adjacent passes. No provider fixture or optional ACK policy is needed;
  its experimental sampler base is separate from live-source validation.
- [Live caller integration](caller-cancellation-live-integration-v1): the same
  six passes/18 failures before, 24 passes after and 24 adjacent race passes on
  frozen live sources. Only the hard caller guards differ; the live sampler and
  optional ACK policy remain unchanged.
- [Physical policy contract](physical-window-policy-gate-contract-v1): the old
  `Sized` snapshot gate gives three passes/21 failures; checking actual resolved
  policy gives 24 race passes with the same contract tests. All throughput,
  refusal and calibration gates remain intact.
- [Live policy/caller integration](physical-window-policy-live-integration-v1):
  16 combined checks and 24 repeated policy checks pass under race on one
  frozen live binary. This verifies the adopted test correction and caller fix,
  without claiming another physical throughput run.
- [Physical ACK-policy experiment](physical-h1-ack-policy-experiment-v1): all six
  arms and two failed comparisons remain available. The audit distinguishes
  recoverable control refusals and a stale `Sized` policy check from lifecycle
  ownership, which balances in every arm. Both runs remain uncalibrated and
  below their directional references; no throughput closure is claimed.
- [Full receiver-consumer model](model-receiver-consumers-full-v1): 26 passes/
  four failures, all 824 readings. SDK first-feedback recovery, profile/duplex
  sweeps and RTT growth fail despite a preceding focused pass. This source also
  lacks the accepted cumulative fallback and cannot be adopted.
- [Full restored receiver model](model-receiver-restored-full-v1): 27 passes/
  three failures, all 824 readings, with the accepted cumulative fallback
  present. SDK profiles/duplex and the first RTT-growth interval fail. The
  later invalid-order slowdown correction has separate validation.
- [Full bounded-slowdown model](model-receiver-invalid-slowdown-full-v1):
  25 passes/five failures, all 824 readings. SDK first-feedback recovery,
  profiles/duplex, RTT growth and the 100 Mb/s shared-relay cell fail. Its
  507 scoped race passes establish no complete throughput acceptance.
- [Endpoint pacing review](model-endpoint-pacing-review-v1/review.md): offline
  analysis finds positive final service in all 18 directions below their
  reference criterion and in the severe RTT-growth failure. A cold-only
  fallback correction cannot directly reprice those terminal states; endpoint
  snapshots do not identify every preceding cause.
- [Receiver-ring CPU comparison](service-receiver-ring-cpu-v1): 48 complete
  readings cover controller/statistics reads over reordered, valid, mixed and
  legacy input. Every read allocates zero bytes. Invalid-ring median controller
  cost is 8.611/9.082 microseconds before/after; valid reads are about 0.78
  microseconds. The empty selector and the original two-endpoint fixture error
  remain separate evidence, including all 18 partial readings.
- [Physical duplex ledger preservation](physical-duplex-ledger-preservation-v1):
  replaying one unchanged failed physical log exports zero rows with the old
  pattern and four with the corrected parser. All three arms and the censored
  comparison survive; six parser tests and shell syntax checks pass.
- [Physical H1 first-refusal trace](physical-h1-first-refusal-diagnostic-v1):
  bounded capture identifies the occupied two-slot admission boundary for a
  52-byte pure TCP ACK while resend capacity remains open. All failed/censored
  physical arms remain excluded from capacity attribution.
- [Physical H1 pure-ACK admission root](physical-h1-control-admission-root-v1):
  six race passes/three failures. Both constant and delivery sizing lose the
  owned ACK in all three repetitions; available-capacity and public zero-wait
  controls pass. This is the before proof, not a fixed host-performance result.
- [Campaign qualification reporting](window-retention-reporting): three before
  failures become three passes; service-qualified growth is no longer reported
  as unsized. No performance threshold changes.
- [Retained-policy assertion update](window-retention-policy-assertions-v2):
  51 passes retain exact candidate arithmetic, proof timestamps and hard bounds
  while replacing the superseded ordinary-shrink and advertisement-jump policy.
- [Service carrier scope](window-retention-carrier-scope),
  [largest-window statistics](window-retention-statistics-selection) and
  [binding diagnostics](window-retention-service-binding): independent failing
  roots and preserved positive controls. The combined service-aware policy
  selection passes [81 executions](window-retention-policy-service-v3).
- [Window ingredient ownership](window-retention-binding-owner): 81 passes/3
  failures before and 84 passes after; the ownership assertion is unchanged.
- [First combined race correctness](correctness-retained-service-v3): 416
  passes/3 failures. Both known sampler roots and the subsequently corrected
  ownership defect remain visible on that exact source.
- [Combined sampler/retained correctness](correctness-retained-sampler-v4):
  459 passes/two failures with no race warnings. The new ACK-phase root is
  included before its correction; the retained occupancy assertion also fails.
  All 461 outcomes and 12 service readings remain visible.
- [Retained experiment instrumentation audit](retained-instrumentation-audit-v1):
  56 archived variants match their pinned build evidence: 42 race and 14
  nonrace. The report's 12 adjacent-policy passes are corrected to nonrace.
  The mixed-instrumentation occupancy comparison cannot attribute a source
  regression; matched instrumentation is required.
- [Occupancy instrumentation matrix](occupancy-instrumentation-matrix-v1):
  both sampler versions fail all three race repetitions and pass all three
  nonrace repetitions. The unchanged wall-clock performance assertion returns
  to its original nonrace regression scope. All 18 outcomes/readings include
  the earlier profiles, which cannot establish source causality because their
  instrumentation differs.
- [Retained-window root regression](regression-retained-service-v3): 3,252
  passes, six failures and 25 skips on the owner-corrected source with the
  older sampler. Four failures encode superseded policy assertions and two
  expose the sampler roots; the complete 3,283 outcomes remain visible.
- [Explicit bootstrap controls](window-retention-bootstrap-controls-v1):
  both tests pass before/after, with all compression readings and eight
  finite-relay readings retained. These tests use 256 KiB and 48 MiB openings.
- [Combined retained-window/service model](model-retained-service-combined-v3):
  28 passes/3 failures and all 832 readings. Static mismatch and deterministic
  matrices pass; changing windows, SDK profiles and long RTT growth remain open.
- [Combined sampler/cumulative-pacing selection](model-sampler-cumulative-combined-v1):
  13 passes/4 failures and all 312 readings. The short SDK subset passes;
  changing windows, longer SDK paths and RTT growth still fail. The focused
  long SDK test repeats cells from the original profile test. This snapshot
  precedes the queued-bucket horizon and cumulative carrier-scope guards.
- [Combined gap-qualification selection](model-service-gap-combined-v1):
  identical sources yield four passes/two failures in each race and nonrace
  run, with all 64 readings retained. RTT growth matches its reference across
  all intervals; window reduction and long-path SDK profiles remain failures.
  Later writer synchronization and empty-ring provenance edits are excluded.
- [Combined ACK-phase selection](model-flight-phase-combined-v1): four passes/
  two failures with the corrected six-test selector; changing windows and long
  SDK profiles still fail. The initial five-test selector is retained separately
  on the same binary, for 11 outcomes and 60 numerical rows in total.
- [Combined receiver-delay candidate](model-receiver-interval-combined-v1):
  four passes/two failures and all 32 readings. Exact warm interval correction
  does not close changing-window or long SDK throughput; this candidate is
  isolated and is not a full performance fix.
- [Combined receiver-refill candidate](model-receiver-refill-combined-v1):
  all six targeted throughput checks pass on one nonrace binary, with all
  32 readings retained. Exact interval correction, cold short-pair qualification
  and consistent cycle-start timing close the two previous focused failures.
  Its later full model below has two failures; this focused result is not
  acceptance. This candidate is isolated from the working implementation.
- [Full receiver-refill model](model-receiver-refill-full-v1): 28 passes,
  two failures and all 824 readings on the same nonrace binary as the focused
  checkpoint. Changing receiver windows lose throughput, and one short-RTT
  SDK duplex direction misses its gate despite a higher aggregate rate.
  RTT growth passes in this run. All failures remain part of acceptance.
- [Combined receiver ordering variants](model-receiver-ordering-combined-v1):
  the initial guard has four focused passes/two failures; refined endpoint
  bounds have five passes/one RTT-growth failure. Both repeated ordering
  selections pass all 24 executions. All 64 model readings are retained;
  these snapshots also precede restoration of the cumulative fallback.
- [Incomplete growth race pair](retained-growth-race-pair-incomplete-v2): both
  runs hit 30 minutes. Completed failures/readings are retained without claiming
  a completed four-cell growth comparison.
- [Sender scan cost](send-scan-cost-v1): 36 normal/race benchmark readings over
  three flight sizes, zero allocations and explicit state controls. Both scanned
  functions predate retention; there is no host timing pass threshold.
- [Sampler qualification cost](service-qualification-cpu-v1): 36 nonrace
  benchmark readings over the isolated before/after sources, with zero
  allocations. Estimator reads and ACK publication costs are retained with
  concurrent host load; these are microbenchmarks, not host throughput proof.
- [Receiver-timed ACK publication cost](service-receiver-credit-cpu-v1): 18
  nonrace readings with 128 active RTT tuples throughout every measurement.
  Single-message and 32-message heads retain exact byte accounting and zero
  allocations; timing-history lookup adds visible CPU cost.
- [Real ACK coalescer cost](service-receiver-coalescer-cpu-v1): 27 nonrace
  readings across current live production, proposed timing-credit plumbing
  and the warm corrected sampler. Every operation validates the exact H1 head
  with 128 RTT samples and credits one or 32 envelopes; all rows allocate zero
  bytes. Six initial fixture failures used raw RTT where the expected residence
  must include the advertised compression allowance, and remain archived.
- [Benchmark ledger preservation](benchmark-ledger-preservation-v1): the
  actual old and corrected runner collectors process the same nine-row output.
  The old ledger omits every CPU measurement; the new ledger retains all nine
  and every metric. Parser headers, repetitions, custom metrics and malformed
  rows have deterministic coverage without repeating a workload.
- [Cumulative pacing roots](window-cumulative-pacing-roots-v1) and
  [unchanged SDK subset](window-cumulative-pacing-sdk-v1): 21 passes/six failures
  become 27 race passes. The short SDK test now passes all eight cells; the
  longer positive-service failure remains. All 40 before/after readings remain.
- [Cumulative fallback carrier scope](window-cumulative-pacing-carrier-scope-v2):
  33 passes/three failures become 36 race passes. Shared H1 eligibility,
  sibling ownership, standalone service and positive-rate precedence are
  covered without changing candidate arithmetic or throughput thresholds.
- [Original adjacent policy failures](window-retention-adjacent-original-regression),
  [rejected assertion update](window-retention-adjacent-policy-v1) and
  [corrected assertions](window-retention-adjacent-policy-v2): four old-policy
  failures, then 9 passes/3 failures, then 12 passes on identical production.
  Candidate arithmetic, delivery and the numerical occupancy gate survive;
  the rejected new pacing assertion used a carrier that never invokes H1 pacing.
- [Initial stats/writer race fixture](window-writer-statistics-owner-v1) and
  [deterministic access-barrier proof](window-writer-statistics-owner-v2):
  the first fixture misses one teardown reproduction. The corrected identical
  barriers reproduce both races in all three fresh processes; three passes/six
  failures become nine passes, with another 60 adjacent passes. A retained
  physical writer proves external retirement cannot block a statistics copy.
- [Original focused race regression outcomes](correctness/outcomes.txt), with its
  [build manifest](correctness/manifest.json) and [status](correctness/status.json).
- [Failure-before reproductions](failure-before): ACK residence/sampling,
  message limits, gap wakes, SACK overflow, admission wakeup, inner TCP loss,
  replay budget admission, TUN handoff, pacing and upload shutdown ownership.
- [Server integration trials](server-integration.txt): all six H1/H3 variants
  and all three upload/download throughput trials. Environment/configuration
  logs are excluded; only test outcomes and performance lines are retained.
- [Server unit race and relay pool-balance checks](server-checks.txt).
- [Retained-window server source review](server-retained-window-source-review):
  stable review of 205 Go files in `server/connect` and `server/proxy` at
  `154f575c`, with no direct consumers of the new estimator fields. This is
  source compatibility evidence only; no new build or integration pass.
- [Adjacent root-cause audit](../THROUGHPUTFIX-PR2.md#12-adjacent-root-cause-review-and-deterministic-completion-gate): known-limit fallbacks, changing capacities, idle gaps, duplicate sampling paths, overflow and local pacing in RTT. Nine added deterministic cases are included in the focused race selection.
- [Model run before the adjacent review](model-before-adjacent-review/provenance.json): failed immediate-ACK mismatch cells, with every paired reading retained.

## Pilot ledger index

Every extracted reading and comparison is retained in its run's `ledger.jsonl`.
`provenance.json` records the source-log hash, result, fixture stage and row
count. Runs made before the manifest-producing script have no source/binary
manifest; their role is diagnosis, not final capacity evidence.

| Directory | What it establishes or exposed |
|---|---|
| `packet-pilot-1`, `packet-pilot-2`, `packet-pilot-3` | Early host-clock FIFO calibration; includes a failed pilot. |
| `model-pilot` | Failed model cells before pacing/warmup corrections. |
| `model-before-adaptive-pacing` | The 36-cell matrix with the target-rate pacer; the later slow-link test exposed the missing service-rate control. |
| `tcp-pilot` | Initial upload stalls before the pure-ACK TUN handoff fix. |
| `tcp-handoff` | Handoff corrected; the per-packet NAT fixture still refused admissions. |
| `tcp-batched` | Batch-preserving NAT admission; short samples still included startup. |
| `tcp-jitter` | Bounded timer-lateness correction. |
| `tcp-pacing-control` | Three-second pacing on/off comparison at 100 ms/eight flows. |
| `tcp-settling` | Ten-second comparison exposing the inner TCP startup ramp in interval readings. |
| `tcp-shutdown-failure` | All 64 measured arms completed; teardown failed with 123 pooled roots outstanding. Preserved as **failed validation**. |

A/A drift over 10%, a ceiling below 90% of configured link rate, missing
controls and stalled flows are retained as censor reasons. A process pass
does not turn a censored comparison into proof of an optimum. In particular,
the unbudgeted default gVisor buffer range limits the original 100 ms
single-flow instrument.

Reproduce with [tools/throughput-fix-2.sh](../tools/throughput-fix-2.sh), using
a fresh output directory outside Git worktrees (the default is temporary).
It copies repository inputs and compiles/runs inside that snapshot, so source
inspections cannot read later working-tree edits. It records source and binary
hashes, snapshot verification, safe experiment environment, every reading and
the test process's exit status. External local module replacements and host
services remain live dependencies. The canonical source digest includes the
embedded SDK profile fixture. Earlier archives predate the snapshot correction;
`regression-source-idle` explicitly classifies the observed runtime-source drift.

The `pacing` mode measures full-ring estimator CPU and allocation cost, including
zero-hold controller reads, read-only statistics and send-worker scans at three
retained-flight sizes. Its separately normalized
`benchmark-rows.jsonl` records ns/op, bytes/op, allocations/op and the estimated
rate; a virtual-time throughput pass cannot establish this CPU cost.

## Burst and statistics follow-up

The report separates each source checkpoint; a result from an earlier binary
does not validate later pacing edits. `correctness-burst`, `model-burst`,
`regression-burst` and `tcp-short-burst` retain the first explicit-burst checkpoint,
including its failed compression-ablation control. The `*-burst-ring` campaign
retains the later checkpoint, including failed comparisons and the obsolete
send-item size assertion. Its test-only correction explicitly adds the new
eight-byte burst identity to the exact size guard.

The [failure-before index](failure-before/README.md) maps new deterministic
reproductions to their failed outcomes and current test names. Final confirmation
results and source boundaries belong in the report, including any later fix
for reordered samples after a ring reset.

The completed `*-burst-ring-final-service-epoch` archives retain the full
`b5b40736` campaign: passing correctness, model and root regression, plus failed
host comparisons with control exclusions. The subsequent `controlled-epoch-final`
archive records the natural-drain correction, its deterministic failures and
nine passing focused model pairs. Its adaptation diagnostics remain separate
from settled-throughput acceptance. `correctness-burst-ring-final-controlled-epoch`
passes all 156 tests under the race detector on source `f60cf11d`.
The same source's `model-burst-ring-final-controlled-epoch` archive now retains
21 passing tests, 272 paired cases and all 508 ledger rows. These runs started
immediately under recorded concurrent host work.

`correctness-burst-ring-rtt-resize` passes 160 race-enabled tests on source
`56eec7b1`, after preserving delivery checkpoints across bucket-duration changes.
Its `model-burst-ring-rtt-resize` full run completes 21 passing tests and one
slow shared-service failure, with all 510 readings retained. The preceding
passing model does not supersede this later failed result.
`rtt-resize-evidence` retains the direct and intermediate failures, 93 focused
race passes, 23 passing model pairs and the complete recovery comparison rows.

`host-feedback-controlled-epoch` retains eight induced host readings and a
three-run deterministic failure through the real inner-TCP replay worker.
The original synthetic test source retains its `.go.txt` extension; the corrected
normal regression now lives in `transfer_window_host_feedback_test.go`.
These single-arm diagnostics are not A/A-bracketed acceptance comparisons;
their controls, scheduling limits and failed outcomes remain visible.

`correctness-source-idle` passes 169 race-enabled tests on source `5605efa7`.
`source-idle-evidence` retains 18 before failures, 276 passing focused race
executions and 50 scoped model readings. The replay correction preserves the
previous service across a proven source pause; fresh slower evidence still
replaces that hold. Full model and host acceptance of this source remain open.
`source-idle-followup` retains the complete before/after host brackets and exact
SDK duplex repeats on separately pinned overlay binaries. Both host brackets
pass but record zero sampled NAT replay events, so they do not prove repair of
the replay root. One of three SDK repeats still fails after the correction.

`sdk-settings-current` records eight constructor profiles through the sibling
SDK's real sizing helpers. Its manifests pin SDK `7fe75c69` and connect source
`5f28f158`; the capture includes a newer recovery test and is not represented as
the `f60cf11d` model source. It verifies resolved settings on the host, including
selected mobile policy, without making a mobile-runtime performance claim.

`sdk-transfer-profiles` expands the capture to 11 profiles and 40 fields,
including explicit mobile H1 queues and lanes. It pins SDK `7fe75c69` and
connect source `56eec7b1`. The embedded test fixture carries its own digest.
`sdk-transfer-model-first` retains all 300 readings from 150 paired Transfer
cases on source `3242678e`: the one-way test passes and the bidirectional test
fails in the mobile H1 provider's return direction. This run precedes the
source-idle correction and stronger per-direction/calibration checks; it does
not validate those later edits or physical H1/TUN behavior.

`correctness-pending-probe-observer` passes 177 race-enabled tests on source
`538e6248`. `pending-probe-observer-evidence` retains 24 exact pre-fix failures,
321 focused race passes and 37 scoped model pairs. The correction distinguishes
pending old ACK bytes from complete or fresh evidence, and prevents public
statistics polling from changing held service. `model-pending-probe-observer`
then passes all 24 tests and 423 pairs with 810 rows on the same frozen source.
Separately forced SDK feedback-recovery failures remain open.

`sdk-transfer-source-idle` retains the recorded 150-pair SDK pass on `5605efa7`.
Its nine reversed duplex pairs later prove miscalibrated: one physical direction
was unlimited. `model-pending-probe-observer` and `model-feedback-cycle` each
have the same 18 affected rows, retained with this qualification. The original
`sdk-transfer-model-first` has no reversed duplex rows and is unaffected by this
specific fixture bug. Earlier outcomes do not erase exact-cell duplex failures.
`regression-source-idle` retains
3,011 passes, two failures and 24 skips, including a runtime-source mismatch
and an independent short-path calibration failure. `short-path-fixture-evidence`
retains forced late-wake failures, unchanged-production virtual-time passes,
12 final race executions and a rejected equal-underfill control. These artifacts
do not establish physical H1 or host performance acceptance.

`regression-source-snapshot` retains all 3,021 root passes, 24 skips and no
failures on `7afd9e4b`. `correctness-source-snapshot` passes all 177 race-enabled
correctness tests on that source. Its production is unchanged from `538e6248`;
the test-source change calibrates the short-path fixture. Both runs compile and
execute inside copied repository inputs. `source-snapshot-evidence` preserves
the synthetic source-drift, inventory and output-boundary checks.

`host-replay-confirmation` retains both full passing throughput brackets and
eight forced actual replay observations. The source-idle ablation collapses the
candidate's rate estimate while the corrected arm holds its preceding rate.
Both throughput comparisons pass, so there is no claimed repair throughput
gain. The legacy helper can exceed actual H1's 8 KiB message cap; this remains
FIFO/userspace-TUN and real-origin diagnostic evidence, not physical-H1
acceptance. Noncanonical overlay keys invalidated an initial compile, which was
rejected before execution; manifests retain the corrected build provenance.

`physical-h1-smoke` retains three actual TLS/WebSocket H1 readings and their
full passing comparison on `b6abfa4e`: 91.469 Mb/s candidate against 91.550 Mb/s
reference. This covers one 100 Mb/s, 0.3 ms added-RTT download with an owned
TCP origin and userspace TUN, Transfer encryption disabled and no server auth.
It includes resolved SDK/NAT/carrier budgets, ACK costs and teardown counters.
Broader physical and native-TUN acceptance remains open.

`physical-h1-fixture-evidence` retains the old single-Pack and missing-context
failures, the final guarded helper handoff and 12 focused race passes from a
verified source copy. The earlier reused-copy test pass failed inventory
verification after a generated bytecode file appeared; it remains explicitly
invalid provenance and is not counted as final validation.

`tcp-grouped-fixture` preserves the corrected generic TCP matrix on `b6abfa4e`:
32 readings and eight comparisons, six accepted and two excluded by reference
calibration, with no candidate failures. The excluded cells are one flow at
100 ms in each direction. `tcp-grouped-capacity` retains a separate 48 MiB
TCP-buffer control with 12 seconds per arm, versus three seconds in the default
run: eight readings and two accepted comparisons. Both use unchanged Transfer
budgets, retain all calibration outcomes and record concurrent host load. Their
FIFO/userspace-TUN scope is separate from physical H1 and SDK acceptance.

`physical-h1-duplex-diagnostics` preserves the first isolated physical duplex
extension: one cleanup interruption and two complete A/B/A attempts, each with
a failed and excluded comparison. All six numerical readings, constructor
settings, ownership outcomes and source/binary pins are retained. References
also stall, so this is diagnostic evidence for further TUN/feedback investigation,
not an accepted pacing comparison. It uses an owned unauthenticated relay,
userspace TUN and disabled Transfer encryption; the final attempt uses one
actual provider dispatcher and fixed synthetic flow ports.

`tun-duplex-root-evidence` retains six expected failures before removing the
redundant TUN endpoint lock, followed by 57 focused race passes. The finite-tail
test observes a real cumulative TCP ACK before reading the delivered bytes.
`correctness-tun-duplex` passes all 182 correctness tests under race on source
`c07140f7`. `physical-h1-duplex-tun-fix` retains all three readings from the
unchanged duplex A/B/A after that correction. It remains failed and excluded:
reference calibration/drift and provider return-control refusals invalidate
acceptance. Complete counters and one invalid pre-compilation setup attempt
are retained; no throughput gain is claimed.

`sdk-ack-tail-v3-evidence` retains 14 complete diagnostic runs, all 122 numerical
readings and 170 normalized outcomes. The candidate remains unlanded/rejected:
improved SDK feedback recovery comes with a real RTT-growth regression. Passing
unchanged-production controls and all failed candidate readings stay visible.

`sdk-feedback-cycle-v10-evidence` retains 22 complete runs, 167 numerical
readings and 1,663 outcomes (1,625 passes and 38 failures), including pre-fix
roots, rejected v9 and unchanged controls. The final focused, race and declared
recovery/phase controls pass, but subsequent timestamp-partition review finds
another deterministic defect; this archive remains provisional.
`correctness-feedback-cycle` records 213 race passes and one exact struct-size
failure on source `914a2a72` from the eight-byte delivered-credit word.
The subsequent correction removes that per-record field
by passing credit directly to the compressor and keeps the original size test.
`ack-credit-without-record-growth` retains all 40 passing ACK/size race checks
on source `f9c0626b`. Full-source pins and unsuccessful outcomes are retained.

`sdk-feedback-cycle-v11-evidence` retains 12 runs, 111 readings and 1,278 outcomes
(1,257 passes and 21 failures), including the summary-accounting roots and all
new long-path failures. `v11-feedback-summary-roots` separately passes five
roots under race on the combined direct-credit/v11 source. These are scoped
correctness results; small-window and long-RTT performance remain open.
`model-feedback-cycle` preserves all 814 v10 readings, 21 passes and five failing
tests. `regression-tun-duplex` passes 3,026 root tests with zero failures and 25
skips on the separately pinned TUN-only production source.

`compression-residence-isolated-control` records three race passes and six
complete readings with both arms held to the advertised ACK interval. The
original throughput and residence gates are unchanged. Its provenance explains
that the custom runner omitted service rows from its initial ledger; all six
rows were recovered from the original hashed log without rerunning the tests.

`duplex-fixture-bounds-before` retains three missing-serializer failures on a
declared 100 Mb/s link. All nine observed rows remain; the fourth declared cell
in each attempt never executes after the preceding assertion fails. Earlier
`Upload && Bidirectional` performance rows cannot establish calibrated duplex
capacity. The corrected fixture preserves both physical rates and adds an
upper bound for every SDK direction. `duplex-fixture-bounds-after` retains all
six passing race executions and 24 readings on source `00093323`, covering both
orientations, one/eight flows, and upward/downward rate changes with zero
measured relay drops. This fixture validation does not replace a corrected SDK
performance campaign.

`long-drain-root-failure-before-evidence` preserves nine complete runs, 28 full
numerical readings and 44 outcomes (21 passes and 23 failures). It includes the
configured drain-cap/mixed-mean roots, the actual-worker premature probe retry,
counterfactuals and all declared long-path failures. The three permanent tests
each fail three times before correction; invalid early fixtures are explicitly
excluded as root proof. Trace outcomes are normalized and raw traces remain
local. This archive does not validate the later candidate or close long-path
performance acceptance.

`server-bootstrap-recheck` records one normal environment bootstrap on server
`0522f3f6`. It stops at managed-launcher readiness before building a binary or
running a test. No database tier, credentials, hosts, launcher state or policy
was changed; no further attempt is planned without external-state change. The
archive retains only sanitized phase/outcome metadata and the raw-log hash.

`window-mismatch-v4-arrival-failure-before-evidence` retains nine frozen v4 test
executions (three passes and six failures), nine failed assertion rows, and
three complete numerical observations. Byte/RTT worker ordering and a
compression-only change can reprice earlier delivery; the older-RTT control
passes. Source, overlay, test and binary hashes remain explicit. This is
failure-before evidence for an isolated candidate, not acceptance of its
preceding performance passes.

`long-drain-v2-recovery-evidence` retains 19 complete runs, 102 numerical
readings and 2,278 outcomes (2,250 passes, 28 failures). Corrected worker
barriers give three race-enabled failures before the probe timer correction
and 444 passing race executions after the isolated v2 changes. Three separate
race-enabled failures establish the different-message invalidation defect.
The old eight-warning fixture diagnostic is retained but is not accepted race
evidence. All 400 ms and 1.2-second throughput failures remain unresolved.

`pacing-estimator-baseline` measures the full-ring estimator on frozen source
`17238935`: continuous, hold and statistics-only hold reads take 189.4, 173.9
and 175.4 ns/op respectively. All three retain 12,500,000 B/s with zero bytes
and allocations per operation. This baseline supports later CPU comparisons,
not throughput acceptance. The generic model ledger is empty in this mode;
the separately hashed benchmark rows preserve all three observed results.

`pacing-estimator-v6-rejected` changes only the pacing file in that frozen
baseline. Continuous/hold/statistics-only reads measure 209.3/5,839/5,827 ns/op,
with identical rates and zero allocations. The hold paths are roughly 33 times
the baseline in this paired under-load sample. This is CPU-cost evidence for
a rejected candidate, not throughput acceptance. The receiver-feedback work
uses these benchmarks to check that simpler evidence also has bounded CPU cost.

`ack-metadata-compatibility-inventory` records the pre-extension runner
selections and strict layout, codec, ownership and encoded-response guards.
It is a read-only inventory with source pins and contains no test execution.

`ack-receiver-delay-wire-checkpoint` validates optional actual receiver ACK
delay on source `8ae48d8a`: 24 race passes and two allocation passes. It covers
both codecs, legacy decoding, presence/zero/max values, malformed fields,
decoder reuse and encoded reservation checks alongside ACK ordering/pacing.
Exact send-item/sequence-ACK/compact-ACK sizes remain 584/96/104 bytes; the
decoded frame owner is 680 bytes. This is a wire checkpoint before receiver
stamping and sender timing consumption, not a throughput correction.

`receiver-ack-timing-root-evidence` preserves the first isolated timing roots:
the two receiver wire tests fail six times before stamping and pass six times
afterward; the blocked sender-worker test fails three times before immediate
RTT publication. All selected runs have zero race warnings. The missing-wake
fixture diagnostic and provisional sender-v2 pass remain explicitly separate.
This archive establishes queue/callback timing and the worker-ordering defect;
it does not validate the combined sender/service estimator or throughput.

`receiver-timing-estimator-cpu` measures the frozen shared timing prototype
with all 128 timing slots populated. Seven benchmarks pass with zero bytes
and allocations per operation. Continuous, held and statistics-only metadata
reads take 472.7, 405.4 and 413.5 ns/op; timing publication takes 285.2 ns/op.
The same candidate's legacy service reads take 196.2, 176.0 and 172.7 ns/op.
All six service-read rows retain 12,500,000 B/s. These are CPU measurements
under concurrent work, not combined throughput acceptance; earlier baseline
readings are explicitly unpaired context.

`receiver-timing-final-focused-evidence` retains six runs, 342 outcomes and 45
numerical readings, including all 21 failed outcomes and two explicitly
excluded fixture diagnostics. The final timing implementation passes 162 race
executions. This precedes the shared-source lock-scope adjustment and later
carrier/baseline corrections; it is not a full performance pass.

`receiver-timing-correctness`, `receiver-timing-model` and
`receiver-timing-regression` share copied source `733ee2d2`. Correctness records
257 passes, three known drain failures and two unlocked-fixture race warnings;
all 23 receiver-timing prefix tests execute and pass. Model records 21 passes,
six failures and 818 ledger rows. Regression records 3,101 passes, the same
three drain failures and 25 skips. These complete campaigns retain every
outcome and started under concurrent work. The later fixture lock correction,
carrier guard and unloaded-baseline work are separate source boundaries.

`receiver-timing-carrier-evidence` retains three deterministic failures before
the H1-only shared-timing guard and 42 race passes afterward. The guard prevents
an H3/mixed carrier from borrowing H1 sibling RTT while preserving that sibling
history. Its separate 1.2-second RTT-growth test still fails; the numeric summary
is retained. None of these isolated runs is attributed to `733ee2d2`.

`receiver-timing-observation-once` preserves the retry double-counting root:
all four preparation/success/failure/unreliable variants fail in each of three
executions, changing one 10 ms observation into two with a 15 ms mean. Keeping
the consumed message state passes 72 focused race executions. The initial
first-case-only diagnostic is retained separately and does not claim coverage
of the other variants. No performance acceptance is implied.

`legacy-drain-v3-final-evidence` retains ten runs, 1,723 outcomes and twelve
model readings. The final corrected fixture/production selection passes 534
race executions; all 25 failed outcomes and the separately excluded earlier
fixture diagnostics remain visible. This establishes bounded legacy drain and
exact-probe recovery behavior, not complete long-path performance.

`receiver-hybrid-baseline-review-evidence` retains the ten pinned handoff logs:
208 passes, 24 failures, eight full service readings and zero race warnings.
The historical two-warning fixture run is listed separately. The final focused
selection passes 192 executions. The long comparison includes the broad
consumer's aggregate-only pass with intervals of 0, 0 and 270.664 Mb/s; that
release does not establish sustained capacity on the 100 Mb/s serializer.

`receiver-hybrid-sdk-bidirectional-compare` retains all 108 readings and 54
comparisons from three fixed binaries on identical base inputs. The broad and
prior raw consumers fail three gates and one gate respectively; the hybrid
passes all eighteen comparisons. `receiver-hybrid-window-mismatch-ledgers`
retains 36 full rows from the same comparison: all three variants fail the
2 MiB to 64 KiB shrinking-window cell. Neither result supersedes the other.

`paired-probe-recovery-final-evidence` retains five runs, 318 outcomes and 27
readings. Exact head/SACK worker roots fail six times before the common raw
residence bound; the final selection passes 150 race executions. All twelve
failed outcomes remain, including the long-path performance failure. This
correction does not prove that retries preceding the probe permit a clean drain.

`correctness-paired-probe-hybrid` validates the integrated source `fffee019`:
291 passes under race, zero failures/skips or race warnings, and eight model
readings. The complete copied inputs, binary and source digests are pinned.
This is correctness evidence; the small-window and long-path capacity gates
remain open. The run began immediately under concurrent host work.

`regression-paired-probe-hybrid` retains the complete root regression on
`fffee019`: 3,133 passes, two failures, 25 skips and no race warnings. The
silent-lane case fails admission at message 1,438; the storm control fails to
reproduce its precondition. Neither failure is removed by an isolated replay.

`receiver-hybrid-other-regressions` retains all 234 readings from the frozen
hybrid before the common probe correction: static mismatch and finite shared
relay fail; repeated drains pass. Six small-sender-window cells lose more than
ten percent of their matched reference, and only the slow shared-relay rate
fails its gate. This is separate from the later service-credit candidate.

`small-window-feedback-root-evidence` preserves 107 outcomes (88 passes and
19 failures) and nine model readings, with no races. The blanket gap filter is
rejected: it breaks eleven complete slow-cycle/drained-train checks, beyond two
old consumer-fixture assumptions. No complete shrinking-window pass is claimed.

`old-tail-receipt-oracle-design-evidence` preserves three passes, six failures,
three route observations and two known-service model readings. Logical ACKs
cannot prove the latest retry copy arrived, and a newer ACK on another route
cannot drain an older physical lane. The oracle run still fails; no attempt
receipt protocol or production change is claimed by this design evidence.

`service-credit-arrival-final-evidence` retains 1,006 passes, eleven failures,
fifty service readings and three allocation-free CPU benchmarks. The final
focused selection passes 738 race executions, including three repeats of the
original finite-relay capacities. The supplemental common-probe control fails
all six static small-window cells before service credit; the same extracted
cells pass afterward. The initial full-window-scan experiment and perturbing
trace remain explicitly excluded diagnostics. No dynamic-window or long-path
acceptance is implied.

`correctness-service-credit` validates integrated source `8802ba4b`: 299 race
passes, no failures/skips or race warnings and eight model readings. Its frozen
source includes the three actual-worker service-credit roots and five adjacent
index/ownership tests. Performance transitions require their separate gates.

`static-mismatch-service-credit` retains the original complete static matrix
on frozen `8802ba4b`: all 108 comparisons pass, with all 216 service readings.
The normal run took 352.32 seconds under concurrent work. A preceding compile
attempt from the snapshot parent is recorded separately as setup-only, with no
binary or test readings; it is not a matrix outcome.

`shared-raw-recovery-v3-review-evidence` retains the scoped first-physical
clock, shared raw residence and heap-order review. The final selection passes
99 race executions; the final-shaped before selection records three passes and
27 failures. Rejected broad-RTO loss roots, the excluded race fixture and oracle
readings remain separate. Both unmodified long-path gates still fail, and the
append-only silent-lane replay refuses message 1,335 after 77.02 seconds.

`correctness-shared-raw-recovery-v3` validates source `44459e42`: all 305 race
tests pass, with no failures/skips or race warnings and eight model readings.
This precedes the subsequent test-only storm-precondition correction and any
new retry-clock correction.

`storm-fixture-precondition-evidence` retains the stale control's three failures,
three constant-window control passes, and nine final race passes. Eighteen
normalized observations show retries with the historical defer disabled,
zero retries with it enabled, and zero retries/deferrals under the default
pacer. Both production mechanisms are checked independently. Ignored overlay
and virtual-fixture setup attempts remain excluded; this test-only correction
does not resolve silent-lane admission.

`small-window-ingress-comparison-evidence` retains 37 frozen diagnostic runs:
34 completed and three excluded, with 354 passes, 63 failures, 124 service
readings and no race warnings. No candidate is accepted: corrected ACK timing
is rejected by a physically valid reverse-compression control, and the
receiver-ingress prototype still fails the unchanged long-path gate.

`receiver-queue-sampler-controls` retains the frozen f9 before/after diagnostic
controls under the race detector, three repetitions each. Before records 15
passes and six failures; after records 18 passes and three failures. The after
overlay passes the max-span paced-capacity root, while the buffered-burst root
still fails. This is sampler-level evidence only: it does not exercise an
actual forced-Client queue, propose a production change, or establish
throughput acceptance.

`receiver-feedback-wire-design` retains a read-only proposal and one passing
unknown-field sizing test with eight maximal wire rows. The optional varint
tuple reaches 40 bytes; encrypted v2/legacy rows use 294/297 of a 320-byte
reservation, and 5,417/5,420 of a 5,440-byte reservation with 512 evictions.
The current decoded-pack owner is 1,000 bytes, and naively retaining the
complete tuple in its existing envelopes would exceed that charge. No schema,
codec, lifecycle, consumer-eligibility, or throughput behavior is accepted by
this design evidence.

`retry-physical-time-review-evidence` retains the retry-clock review: the
final shaped before selection has twelve passes and twelve failures, while the
final focused race selection has 138 passes. The unchanged virtual silent-lane
cell still fails three times at message 1,445. Intermediate fixture assumptions
remain excluded, and no long-path or host-capacity result is claimed.

`unprovable-drain-review-evidence` retains the constant-time drain review: the
final before selection has nine passes and 33 failures, while the counter-after
focused race selection has 621 passes. The initial carrier expectation and
ambiguous-tail trace remain separate diagnostics. `server-preflight-retry-drain`
records four setup blocks with no Go build or test start; database tiers remain
deferred.

Only numerical ledgers, manifests, provenance and outcome excerpts are part of
the committed evidence. Raw logs and test binaries remain local. The collector
keeps raw-log hashes and copies complete comparison ledgers; it does not remove
slow readings or promote an excluded comparison to a capacity result.

`window-proof-port-evidence` isolates the proved-RTT window-history correction
with the original service sampler: 15 passes/18 failures before, 33 passes
after, then 338 full correctness passes under race on the same after binary.
This is semantic/correctness evidence; separate model validation is required.

`receiver-hybrid-full-v2-evidence` preserves all 818 original model readings
and its 25 passes/two failures. `receiver-hybrid-ownership-affected` retains
the narrower discovery handoff: before SDK three passes/three failures and
after nine passes over 48 affected-cell readings. These remain experiments
with zero wire overhead and a test-only receiver lookup.

`receiver-queue-worker-evidence` retains the actual Client/carrier queue root:
receiver-clock and max-span arms each have six passes/three failures under
race. `receiver-queue-admission-diagnostic` retains the identical worker root
with a narrow queued-read endpoint exclusion: six passes/three failures before,
nine passes after and 75 expanded passes. No kernel/socket-arrival timestamp
or final production/wire acceptance is claimed.

`window-proof-independent-validation` pins the exact `272f95e5` plus
window-history correction: 33 race passes, without experimental drain edits.
`model-window-proof-original-sampler` retains all 818 readings and the same
three failed model tests on the working drain variant with that correction.

`receiver-hybrid-ownership-full-model` retains all 818 readings and 27 passes
on the narrower discovery handoff. The separate Client queue root still fails
that variant. `receiver-queue-admission-affected-model` records eight passes
and 38 readings after queued-endpoint exclusion. Both use a test-only receiver
lookup, so these results do not include new wire cost or establish production
feedback ownership.

`root-condition-final` retains the exact last commit plus 22 production root
tests: 33 passes and 33 failures over three race repetitions, with every one of
the eleven failing tests reproduced each time. `correctness-root-condition-coverage`
retains the preceding expanded full run: 348 passes and seven newly covered
failures. Neither is a clean correctness result.

`receiver-root-replay` retains all four checked-in replay variants: 354 passes,
78 failures, no skips or races, and 120 worker readings. Their source and runner
are under `testdata/throughput_root_cases` and `tools/`. The early-handoff,
forward-delay, Client/carrier buffering, bucket-phase, cold-start and genuine
increase failures remain visible.

`root-condition-complete` adds the eleven previously uncommitted drain roots
to the production baseline: 33 tests repeated three times on `17780670` plus
tests yield 39 passes and 60 failures, with no skips or races. Each of the twenty
failing cases reproduces three times. This baseline excludes the working drain
correction; its liveness passes do not establish wider performance acceptance.

`receiver-queue-admission-full-model` retains 27 passes and all 818 readings.
The upstream-buffer roots still reject that variant. `receiver-carrier-buffer-hybrid-review`
preserves the earlier first-position controls; later bucket-shifted failures
show why those isolated passes do not establish acceptance.
