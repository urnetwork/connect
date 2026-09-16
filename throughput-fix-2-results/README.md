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

`sdk-transfer-source-idle` retains the complete 150-pair SDK pass on `5605efa7`.
It does not erase exact-cell duplex failures. `regression-source-idle` retains
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

Only numerical ledgers, manifests, provenance and outcome excerpts are part of
the committed evidence. Raw logs and test binaries remain local. The collector
keeps raw-log hashes and copies complete comparison ledgers; it does not remove
slow readings or promote an excluded comparison to a capacity result.
