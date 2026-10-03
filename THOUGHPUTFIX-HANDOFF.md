# Throughput-fix-2 handoff

## Current checkout

- Repository: `/Users/brien/urnetwork/connect`
- Branch: `throughput-fix-2`
- Validation base HEAD: `f903f59a96e629f8b1dfa5849d7a3198aaef9920`
- The worktree is intentionally dirty. Preserve the existing edits and do not
  stage the whole tree: `throughput-fix-2-results/` contains many archived
  experiments and run logs.
- Core validation now passes. The remaining research scope is the host/physical
  comparison work, later `NetworkQualityChanged` wiring and the user-deferred
  database-backed integrations. The report and plan distinguish these from
  completed deterministic core fixes.

## User decisions that are binding

- Tests representing a genuine, programmed connection-quality change may be
  deferred until `NetworkQualityChanged` exists. Add that signal at the actual
  modeled path transition, then restore those tests with the signal. Do not
  use the signal to hide estimator, ACK, ordering, capacity, or unnotified
  congestion bugs.
- The learned window may grow from valid evidence and must not shrink unless a
  future `NetworkQualityChanged` event requests remeasurement. Pacing can adapt
  downward between events when sustained feedback proves congestion.
- All ACK compression has one cumulative head ACK and bounded oldest-first
  SACKs above the head. The head absorbs pending SACKs at or below it. ACK
  metadata/size and pacing tests already cover this contract.
- Run tests under the current concurrent host load. Do not wait for system
  quiescence. Record that context in every new result.
- The database-backed `server/connect` and `server/proxy` integrations are
  deferred as previously authorized. Do not change credentials, host setup, or
  the sibling server checkout.
- Terra medium runs tests/builds; Astra max performs debugging/fixes. The
  current `/root/terra_validation` and `/root/astra_debug_fix` agents use that
  split. Keep it when handing the remaining work to fresh agents.

## Current production checkpoint

These pins identify the current production source that passes the full core
model, race correctness, root regression and scoped server selections:

| File | SHA-256 |
| --- | --- |
| `transfer.go` | `7227fe36af8e6d18480bb48c583eeb08e2511dd5265ae964a74f5922de526584` |
| `transfer_window_pacing.go` | `6d1fc9afcf378ff01ad419371408717745bd95be529c8579eb4fda7c5e8b3fda` |
| `transfer_window_receiver_rtt.go` | `416d47e5e55a4eddc720c1c7a34cc615ae6b21db52a7db3fa6f1ef500ed02433` |
| `transfer_window_service_credit.go` | `8cc0f24467b1ffc0d16b96517bc39e8f61afaf2e5f77ea9718b46dc5638c50c4` |

The discovery floor, held pacing rate, exact-head prefix timing restriction,
accepted SDK timing contracts, caller-cancellation and physical H1 policy-gate
fixes are live. Preserve them when comparing snapshots. The common raw delivery
ring and warm `max(service, aggregate)` variant remain historical experiments.

### Validated held-pacing correction

`transfer_window_pacing.go` currently has SHA-256
`6d1fc9afcf378ff01ad419371408717745bd95be529c8579eb4fda7c5e8b3fda`.
A hold-release correction tolerates RTT inflation within the
existing maximum burst duration. A backlogged read or longer queue still
allows downward pacing. Blind discovery continues to end at its original
queue margin. No new setting or byte permission is introduced.

`TestWindowPacingOnePermittedBurstKeepsHeldRate` deterministically fails before:
a 10 ms reverse burst inside the existing 20 ms allowance clears a
137,500,000 B/s hold to 111,219,709 B/s despite no forward backlog. The matching
beyond-burst/drained-flight control and focused repeated race checks pass
with the candidate. Five repeated short-duplex/capacity selections pass,
including both device roles, discovery at capacity, an unnotified capacity
drop, compressed feedback and repeated drains. The two new roots pass five
race repetitions. A third adjacent control now requires excess forward
flight to bypass and replace the hold at 0.95 service.

The matched pair is frozen at `/tmp/throughput-fix-2-burst-hold-pinned-v1`.
Only `transfer_window_pacing.go` differs; both arms include identical tests
and a frozen local `glog`. Terra used the commands in
`/tmp/throughput-fix-2-burst-hold-run.py` for five root definitions times three
race repetitions and the unchanged SDK test times five. The matched root
result is 12 passes/three failures before and 15 passes after. Both SDK arms
pass five repetitions, so the pair proves the component policy correction,
not throughput uplift or a deterministic reproduction of the entire earlier
end-to-end failure. The new full core model passes 36 definitions/846 readings;
combined correctness passes 512 race definitions. Full root regression passes
3,345 tests with 25 existing skips and no failures. Research plan section 36
describes the contract.

Completed immutable evidence:
`throughput-fix-2-results/service-pacing-burst-hold-v1`, manifest SHA-256
`4c1def36687e78d46f5336b5ef5c2f738376243ef3429b12eea5a83d6e4dc815`.
The candidate correctness and matched after source is
`a36793a9871306b3acbb87e2f0ddeb1d79cecaebf26c795c518acdf1ae27c04b`.
The full model source is
`5099a11c24e26b9f4bd9ed0f8da8ce067bbb6d50521f7c52250898bfad6aaad4`;
it preceded the third test addition but has identical production pacing.

## Latest validation state

- The live discovery/held-pace policy passes the scoped constrained 400 ms SDK
  cells, around 130.94 Mb/s against 130.95 Mb/s reference.
- The remaining short duplex failure was reproduced in both retained H1
  device roles. Applying a cumulative head's positive receiver wait to its
  newly credited prefix was still live. The exact-head restriction changes
  both roles from repeated SDK gate failures to improved throughput without
  changing the warmup, per-direction 90% gate or physical queue limits. The
  pinned after pair still fails once: non-providing upload 855.296 Mb/s versus
  957.2352 Mb/s reference. The hold-release candidate above addresses the next
  deterministic controller condition and passes fresh model/correctness runs.
  Full root regression also passes on that candidate.
- Twenty prefix, SDK receiver-interval, cold-refill and cold queued-timing
  definitions pass three race repetitions. Forty adjacent ACK compression/metadata definitions
  also pass a race run. See research plan section 35 for the root and two
  repaired cold-test expectations.
- The post-prefix `model-core` run at
  `/tmp/throughput-fix-2-terra-model-core-current` passes 35 definitions and
  842 ledger readings. This is the earlier exact-head checkpoint, whose pacing
  file was `b6acc02099e55a24d7f73663bd5e2d97d88fc85f0b39ac2404cada74cb6ef755`.
- Combined correctness and full root regression pass on that exact-head
  checkpoint, source `90606e4f81f37612a305d02a8ff44cc859005b6f7b632ade27bfce997cdf32eb`.
  Regression has zero failures and 25 expected skips. The run at
  `/tmp/throughput-fix-2-terra-regression-current` started at load averages
  `[5.33, 7.02, 8.58]` and finished at `[10.24, 10.39, 9.05]`, with no
  quiescence wait. These results must not be attributed to the later candidate.
- The exact prefix-only source pair is frozen at
  `/tmp/throughput-fix-2-prefix-live-pinned-v1`. Terra built and ran both arms;
  race roots improve from 45 passes/15 failures to 60 passes, while the SDK
  definition changes from three failures to two passes/one failure.
  Only `transfer_window_service_credit.go` differs:
  before source `49938042f7187e7090352abb4fa64ac2dcb04f42d4c070232fb60fd37a4ff5e0`,
  after source `90606e4f81f37612a305d02a8ff44cc859005b6f7b632ade27bfce997cdf32eb`.
  Archive: `throughput-fix-2-results/service-receiver-prefix-live-sdk-v1`.
- Final root regression passes 3,345 tests, with 25 existing skips and zero
  failures on source `a36793a9...`. Archive:
  `throughput-fix-2-results/core-regression-burst-hold-final-v1`, manifest SHA-256
  `f356a096744cf2d65ba0c5eaf5249b77f0e7df07a0c4a0c8f6d54298c8c4fdc1`.
- Final scoped server regression passes all 35 connect race tests and 28
  proxy nonrace tests. Archive:
  `throughput-fix-2-results/server-scoped-configure-final-v1`, manifest SHA-256
  `187f70fa70622278a566acebc6ea3908008832e19b21339d0ef8298acdc3b7d8`.

## Quality-change phase split

`tools/throughput-fix-2.sh model-core <output>` selects the original model
definitions but excludes exactly these three explicit propagation-transition
tests and writes `deferred-tests.json`:

1. `TestWindowPathAckTailRoundTripGrowthControl`
2. `TestWindowPathServiceRoundTripChanges`
3. `TestWindowPathServiceRoundTripGrowthBeyondOldRing`

The original `model` mode remains unchanged and includes all definitions. The
selection audit is archived in
`throughput-fix-2-results/model-core-deferral-v1` with manifest SHA-256
`07dc1a293d9abd40573f8c49de89cd5cac645b1d0b9cc7ae8b17bf46297a6415`.
No `NetworkQualityChanged` API or event plumbing has been implemented yet.

## Scoped server regression environment

`server/connect/test.sh` sources `server/test-env.sh`, which combines export
configuration with launcher/PostgreSQL/Redis preflight. The original runner
therefore stopped before even its non-database tests on an unreadable launcher
owner. The runner now has a narrow audited configuration-only adapter for the
35 connect and 28 proxy synthetic ownership/lifecycle tests. It preserves the
exact `test_env_configure` function and original source path, pins reviewed
environment/helper/test hashes, emits exact test names and records that no
service preflight ran. All integration modes retain the original full
preflight. Five synthetic adapter tests exercise failed service readiness,
source/function drift, widened patterns and rejection of an unreviewed
nonrace mode.

The next native build stopped because the local Xcode license is unaccepted.
No tests ran in that attempt, and no license/host state was changed.
`CGO_ENABLED=0` is supported by `server/Dockerfile`; these selected tests use
no native resolver/cgo API. Go 1.26.7 also preserves `-race=true` with this
setting: a diagnostic binary's build metadata confirms both settings and all
35 connect tests pass. The two audited modes now default to `CGO_ENABLED=0`,
preserving caller overrides and recording it in the manifest. Connect keeps
its original race tier; proxy keeps its documented nonrace tier. Final runs
pass all 35 connect tests and 28 proxy tests. The archived binary metadata
confirms both settings. Integration modes and the sibling server checkout
are unchanged.

The snapshot harness also lacked the ledger parser required by the runner's
hash guard. Its fixture now copies the dependency and forcibly changes the
live original parser during the Go test; post-run parsing still succeeds from
the frozen source. All five snapshot tests and all five environment adapter
tests pass. Their evidence accompanies the final root regression archive.

## What is proven

- Shared cumulative delivery/reservation roots pass after the narrow fixes:
  `throughput-fix-2-results/window-cumulative-shared-delivery-cold-v2` records
  72 race passes, with equal/unequal two- and four-lane consumers, actual
  queued send items, cumulative heads, duplicates, late SACKs, and controls.
  It is prescribed-training component evidence, not full throughput.
- The common-ring pending-drain boundary bug is fixed in the isolated source:
  `service-shared-aggregate-boundaries-v1` changes 15 passes/3 failures to
  18 passes; all 27 warm controls remain intact. Manifest SHA-256:
  `d8850294e94...` (the full value is in that archive's
  `archive-manifest.json`).
- Receiver-wait timing is restricted to the exact newly credited head; a
  cumulative head cannot retime a relayed prefix. The same-local-hop reorder
  proof and downstream-reorder controls pass after the restriction. The
  accepted contract archive is
  `service-receiver-prefix-contract-correction-v3`, manifest SHA-256
  `592b733d8142878fb641e404540ed9e8797ce639f3a8299673a288b2bac8d9f3`.
  It has 60 race passes and preserves the original underdetermined failures.
- Static long-path controls with real send/ACK workers pass after long,
  predeclared warmups at 400 ms and 1.2 s RTT. Archive:
  `model-static-long-recovery-v1`, manifest SHA-256
  `7426a2006173208f71e7019b230c1c309b90d0e6dccc001f14728a90153394da`.
  These do not replace the short-warmup SDK gate.
- The common-ring CPU comparison has 54 readings, all zero allocations. It
  adds 3,608 inline bytes per service; median real-head work is 1,068 -> 1,444
  ns and estimate work is 1,465 -> 1,838 ns. Archive manifest SHA-256:
  `3d8ff0aa6c95e338b979f2c5559821fdce6eb5689e26d320e009c37e2e928e84`.

## Historical startup diagnosis

The earlier common ring/prefix/guard experiment failed fixed-path SDK Long at
100 ms and 400 ms RTT while SDK Short passed. The later discovery-floor fix
addresses that startup policy; these observations describe the earlier source.

The matched warm-v2 experiment changed only pacing's positive service basis to
`max(ServiceByteRate, AggregateDeliveryByteRate)`, before the existing margin
and target cap. Both arms pass 183 repeated race checks, but both fail SDK Long;
the 400 ms result is approximately 51.622 Mb/s candidate versus 131.035 Mb/s
reference. It is not an accepted fix. Archive:
`window-cumulative-shared-warm-pacing-v2`, manifest SHA-256
`8ef9fa1720fd7ff17d54306750bd6d8559c1562afa22809149f06c09c37b7aed`.

The observational ramp trace found:

- The warm common-rate maximum never activates in either Long cell: aggregate
  delivery is never greater than service.
- The first positive service sample arrives around 210 ms at 100 ms RTT and
  810 ms at 400 ms RTT, close to the one-residence bootstrap rate.
- Pacing then grows at the existing roughly 1.1 multiplier. The RTT remains
  unloaded and the target cap is far above the selected pace, so the failure is
  not the old-service versus common-flight backlog basis.
- At 400 ms, one sample briefly falls from about 1.304 MB/s to 7.4 KB/s near
  1.57 s. The next samples recover to about 1.34/1.60 MB/s, but a reservation
  charged at the low rate remains outstanding for roughly 1.945 s. This is a
  distinct possible stale-reservation/first-refill measurement root and must
  be reproduced deterministically before changing admission policy.

Trace source: `/tmp/throughput-fix-2-sdk-warm-ramp-diagnostic-v1-pinned`.
The trace run used concurrent load and no quiescence wait. Its exact raw log
and parsed `trace-analysis.json` are outside the repository.

## Cold first-refill contracts now in the live tree

`transfer_window_cold_refill_cohort_test.go` now uses confirmed physical offers,
once-only credit and the current admission policy. Unsupported first-refill
silence cannot collapse the supported pacing rate; genuinely slow preoffered
and later sustained-slow trains still lower pacing. Faster and contiguous
pairs must report their exact 267,000 B/s service, while the discovery floor
may legitimately pace above the observed delivery. The two former upper bounds
on pacing contradicted the accepted unloaded-path policy and were corrected.
The original isolated source remains at
`/tmp/throughput-fix-2-cold-refill-cohort-before-pinned` for historical review.

The older 1.57 s low-sample/reservation-debt trace has not been reproduced as a
remaining current-source failure. Before changing FIFO or reservation debt,
require a fresh deterministic reproducer against the current discovery and
held-pace policy. Released-byte debt and pending local reservations have
different ownership; preserve cancellation and physical burst bounds.

## Immediate next work

1. Preserve the completed core/server evidence and the complete prefix-only
   archive with its earlier after failure. The
   source pair is already built and run; do not rerun preparation or overwrite
   either arm. Subsequent candidates need new output directories.
2. Finish the host/physical regression work listed in the report. Full native
   rig reproduction and the deferred database-backed server tiers remain
   separate limitations; do not replace their result with model throughput.
3. Implement `NetworkQualityChanged` in its separate follow-up phase, call it at
   the three deferred model transitions, and restore their performance run.

## Verification commands

For a fresh validation after source changes, run from the repository under
current load. `model-core` preserves the authorized three-test deferral:

```sh
tools/throughput-fix-2.sh correctness /tmp/throughput-fix-2-correctness-next
tools/throughput-fix-2.sh model-core /tmp/throughput-fix-2-model-core-next
tools/throughput-fix-2.sh regression /tmp/throughput-fix-2-regression-next
tools/throughput-fix-2.sh server-connect-deterministic /tmp/throughput-fix-2-server-connect-next
tools/throughput-fix-2.sh server-proxy /tmp/throughput-fix-2-server-proxy-next
CGO_ENABLED=0 python3 tools/throughput-fix-2-snapshot-test.py
python3 tools/throughput-fix-2-server-env-test.py
```

Use a fresh output directory for every pinned run. The runner records source
and binary hashes, load averages, selected definitions and the deferred-test
manifest. Continue without a quiescence wait. Do not interpret the runner
selection audit as a passing performance result.

## Documentation to keep synchronized

- Research, root hypotheses, test contracts and phase split:
  `THROUGHPUTFIX-PR2.md`
- Current evidence and remaining work:
  `THROUGHPUT-REPORT-PR2.md`
- Result index and archive interpretation:
  `throughput-fix-2-results/README.md`
- Runner and selection audit:
  `tools/throughput-fix-2.sh`

The report now records the archived prefix/SDK pair, final core and server
counts, and the explicit three-test deferral until the `NetworkQualityChanged`
call sites are implemented.
