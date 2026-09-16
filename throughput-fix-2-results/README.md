# Window follow-up evidence

Read [the results report](../THROUGHPUT-PR2-RESULTS.md) and
[the peer review/research plan](../THROUGHPUTFIX-PR2.md) for interpretation.
Rates in model rows are virtual-time payload Mb/s. TCP rows are host-clock
payload Mb/s. Server output uses MiB/s; these units are not interchangeable.

## Validation and root-cause evidence

- [Original focused race regression outcomes](correctness/outcomes.txt), with its
  [build manifest](correctness/manifest.json) and [status](correctness/status.json).
- [Failure-before reproductions](failure-before): ACK residence/sampling,
  message limits, gap wakes, SACK overflow, admission wakeup, inner TCP loss,
  replay budget admission, TUN handoff, pacing and upload shutdown ownership.
- [Server integration trials](server-integration.txt): all six H1/H3 variants
  and all three upload/download throughput trials. Environment/configuration
  logs are excluded; only test outcomes and performance lines are retained.
- [Server unit race and relay pool-balance checks](server-checks.txt).
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
zero-hold controller reads and read-only statistics. Its separately normalized
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

Only numerical ledgers, manifests, provenance and outcome excerpts are part of
the committed evidence. Raw logs and test binaries remain local. The collector
keeps raw-log hashes and copies complete comparison ledgers; it does not remove
slow readings or promote an excluded comparison to a capacity result.
